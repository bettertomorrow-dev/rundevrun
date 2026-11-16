// Tests networking tailscale behavior.
package networking

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
)

func TestServeOwnership(t *testing.T) {
	ctx := context.Background()
	status := `{"TCP":{"80":{"HTTP":true}},"AllowFunnel":{"other:8443":true}}`
	var calls [][]string
	ts := tailscaleClient{run: func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if reflect.DeepEqual(args, []string{"serve", "status", "--json"}) {
			return []byte(status), nil
		}
		status = `{"TCP":{"80":{"HTTP":true},"443":{"TCPForward":"127.0.0.1:8443"}},"AllowFunnel":{"other:8443":true}}`
		return nil, nil
	}}
	if err := ts.apply(ctx, "add", managedTLSTarget); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 || !reflect.DeepEqual(calls[1], []string{"serve", "--bg", "--tcp=443", "tcp://127.0.0.1:8443"}) {
		t.Fatalf("commands %v", calls)
	}
	status = `{"TCP":{"443":{"TCPForward":"127.0.0.1:9000"}}}`
	if err := ts.apply(ctx, "remove", managedTLSTarget); err == nil {
		t.Fatal("removed someone else forwarding")
	}
	status = `{"Foreground":{"session":{"TCP":{"443":{"TCPForward":"127.0.0.1:8443"}}}}}`
	if err := ts.apply(ctx, "add", managedTLSTarget); err == nil {
		t.Fatal("ignored foreground session")
	}
	status = `{"TCP":{"443":{"TCPForward":"127.0.0.1:8443","TerminateTLS":"host"}}}`
	if _, ok := rawForward(json.RawMessage(`{"TCPForward":"127.0.0.1:8443","TerminateTLS":"host"}`)); ok {
		t.Fatal("accepted TLS termination")
	}
	if err := ts.apply(ctx, "remove", managedTLSTarget); err == nil {
		t.Fatal("removed modified forwarding")
	}
	status = `{"TCP":{"443":{"TCPForward":"127.0.0.1:8443"}},"AllowFunnel":{"host:443":true}}`
	if err := ts.apply(ctx, "remove", managedTLSTarget); err == nil {
		t.Fatal("ignored funnel conflict")
	}
	ts.run = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("offline") }
	if err := ts.apply(ctx, "add", managedTLSTarget); err == nil {
		t.Fatal("ignored offline")
	}
}

func TestManagedTLSUsesSeparateLegacyListener(t *testing.T) {
	legacy := &routing.Config{Routing: routing.Settings{Enabled: true, HTTPSListen: "127.0.0.1:8443", TLS: routing.TLSSettings{Mode: "provided"}}}
	if got := ManagedTLSTarget(legacy); got != "127.0.0.1:8444" {
		t.Fatalf("legacy collision: %s", got)
	}
	legacy.Routing.HTTPSListen = "127.0.0.1:9443"
	if got := ManagedTLSTarget(legacy); got != "127.0.0.1:8443" {
		t.Fatalf("unexpected fallback: %s", got)
	}
	legacy.Routing.HTTPSListen = "127.0.0.1:8443"
	legacy.Routing.TLS.Mode = "external"
	if got := ManagedTLSTarget(legacy); got != "127.0.0.1:8443" {
		t.Fatalf("external TLS has no listener: %s", got)
	}
}
