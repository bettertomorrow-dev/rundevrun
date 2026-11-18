// ManagedTLSTarget keeps the managed listener separate from legacy TLS.
package networking

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
)

const managedTLSTarget = "127.0.0.1:8443"

func ManagedTLSTarget(legacy *routing.Config) string {
	if legacy != nil && legacy.Routing.Enabled && !legacy.ExternalTLS() && legacy.Routing.HTTPSListen == managedTLSTarget {
		return "127.0.0.1:8444"
	}
	return managedTLSTarget
}

type tailscaleClient struct {
	run func(context.Context, ...string) ([]byte, error)
}

func (t tailscaleClient) command(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if t.run != nil {
		return t.run(ctx, args...)
	}
	cmd := exec.CommandContext(ctx, "tailscale", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tailscale %s failed: %w", args[0], err)
	}
	if out.Len() > 1<<20 {
		return nil, errors.New("tailscale response too large")
	}
	return out.Bytes(), nil
}
func (t tailscaleClient) ip(ctx context.Context) (string, error) {
	b, err := t.command(ctx, "status", "--json")
	if err != nil {
		return "", err
	}
	var status struct {
		BackendState string
		Self         struct{ TailscaleIPs []string }
	}
	if err = json.Unmarshal(b, &status); err != nil {
		return "", errors.New("invalid tailscale status response")
	}
	if status.BackendState != "Running" {
		return "", errors.New("tailscale is not running")
	}
	for _, s := range status.Self.TailscaleIPs {
		ip := net.ParseIP(s)
		_, subnet, _ := net.ParseCIDR("100.64.0.0/10")
		if ip != nil && subnet.Contains(ip) {
			return ip.String(), nil
		}
	}
	return "", errors.New("tailscale has no IPv4 address")
}

func (t tailscaleClient) serve443(ctx context.Context) (json.RawMessage, error) {
	b, err := t.command(ctx, "serve", "status", "--json")
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var status struct {
		TCP         map[string]json.RawMessage `json:"TCP"`
		AllowFunnel map[string]bool            `json:"AllowFunnel"`
		Foreground  map[string]struct {
			TCP         map[string]json.RawMessage `json:"TCP"`
			AllowFunnel map[string]bool            `json:"AllowFunnel"`
		} `json:"Foreground"`
	}
	if err = json.Unmarshal(b, &status); err != nil {
		return nil, errors.New("invalid tailscale serve status")
	}
	for host, allowed := range status.AllowFunnel {
		if allowed && strings.HasSuffix(host, ":443") {
			return nil, fmt.Errorf("funnel is enabled for %s; resolve it before setup", host)
		}
	}
	for _, fg := range status.Foreground {
		if len(fg.TCP["443"]) != 0 {
			return nil, errors.New("tailnet port 443 is used by a foreground Serve session")
		}
		for host, allowed := range fg.AllowFunnel {
			if allowed && strings.HasSuffix(host, ":443") {
				return nil, errors.New("a foreground Funnel uses port 443")
			}
		}
	}
	return status.TCP["443"], nil
}
func rawForward(raw json.RawMessage) (string, bool) {
	var m map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return "", false
	}

	if len(m) != 1 {
		return "", false
	}
	var target string
	if json.Unmarshal(m["TCPForward"], &target) != nil || target == "" {
		return "", false
	}
	return target, true
}
func (t tailscaleClient) apply(ctx context.Context, action, ownedTarget string) error {
	raw, err := t.serve443(ctx)
	if err != nil {
		return err
	}
	target, ok := rawForward(raw)
	switch action {
	case "add":
		if ownedTarget == "" {
			return errors.New("missing managed TLS target")
		}
		if len(raw) != 0 {
			return errors.New("tailnet port 443 is already in use")
		}
		_, err = t.command(ctx, "serve", "--bg", "--tcp=443", "tcp://"+ownedTarget)
		if err != nil {
			return err
		}
		after, err := t.serve443(ctx)
		if err != nil {
			return err
		}
		afterTarget, valid := rawForward(after)
		if !valid || afterTarget != ownedTarget {
			return errors.New("Serve did not report the expected forwarding; inspect port 443 before retrying")
		}
		return nil
	case "remove":
		if ownedTarget == "" || !ok || target != ownedTarget {
			return errors.New("tailnet port 443 is not the recorded rundevrun forwarding")
		}
		_, err = t.command(ctx, "serve", "--bg", "--tcp=443", "off")
		if err != nil {
			return err
		}
		after, err := t.serve443(ctx)
		if err != nil {
			return err
		}
		if len(after) != 0 {
			return errors.New("Serve still reports a port 443 listener; inspect before retrying")
		}
		return nil
	case "unchanged":
		if ownedTarget == "" || !ok || target != ownedTarget {
			return errors.New("tailnet forwarding changed since preview")
		}
		return nil
	default:
		return errors.New("unsupported tailnet forwarding action")
	}
}
