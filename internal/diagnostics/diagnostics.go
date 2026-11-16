// Package diagnostics reports routing and application observations without changing state.
package diagnostics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/localrouting"
	"github.com/bettertomorrow-dev/rundevrun/internal/proxy"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

type Check struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}
type ProxyReport struct {
	Status Check `json:"status"`
	HTTP   Check `json:"http"`
}
type RouteReport struct {
	InstanceID    string `json:"instance_id"`
	Service       string `json:"service"`
	Hostname      string `json:"hostname,omitempty"`
	URL           string `json:"url,omitempty"`
	DNS           Check  `json:"dns"`
	TLSSystem     Check  `json:"tls_system"`
	TLSConfigured Check  `json:"tls_configured"`
	Ownership     Check  `json:"ownership"`
	Application   Check  `json:"application"`
}
type ProjectReport struct {
	ID       string `json:"id"`
	Registry Check  `json:"registry"`
}
type Report struct {
	Projects      []ProjectReport `json:"projects"`
	Configuration Check           `json:"configuration"`
	LegacyTLS     *Check          `json:"legacy_tls,omitempty"`
	Proxy         ProxyReport     `json:"proxy"`
	Routes        []RouteReport   `json:"routes"`
}

type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string {
	if e.Code == "NOT_FOUND" {
		return e.Code + ": project not found"
	}
	return e.Code + ": registry unavailable"
}
func (e *Error) Unwrap() error { return e.Err }
func pass(s string) Check      { return Check{Status: "pass", Message: s} }
func fail(s string) Check      { return Check{Status: "fail", Message: s} }
func skip(s string) Check      { return Check{Status: "skip", Message: s} }

func loopbackAddresses(addrs []net.IPAddr) bool {
	if len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		if !a.IP.IsLoopback() {
			return false
		}
	}
	return true
}
func loopbackAddress(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	n, e := strconv.Atoi(port)
	return ip != nil && ip.IsLoopback() && e == nil && n > 0 && n <= 65535
}
func dial(ctx context.Context, address string) (net.Conn, error) {
	if !loopbackAddress(address) {
		return nil, errors.New("endpoint is not loopback")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return (&net.Dialer{}).DialContext(ctx, "tcp", address)
}
func tlsProbe(ctx context.Context, address, host string, roots *x509.CertPool) error {
	conn, err := dial(ctx, address)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	c := tls.Client(conn, &tls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12})
	return c.HandshakeContext(ctx)
}
func configuredRoots(c routing.Config) (*x509.CertPool, error) {
	if len(c.Routing.TLS.Certificates) == 0 {
		return nil, errors.New("no certificates")
	}
	pool := x509.NewCertPool()
	for _, cert := range c.Routing.TLS.Certificates {
		b, e := os.ReadFile(cert.CertFile)
		if e != nil || !pool.AppendCertsFromPEM(b) {
			return nil, errors.New("invalid certificate")
		}
	}
	return pool, nil
}

func Run(ctx context.Context, store *state.Store, configPath, projectID, worktreeID string) (Report, error) {
	report := Report{Routes: []RouteReport{}, Projects: []ProjectReport{}}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	caller := ctx
	reg, err := store.Read()
	if err != nil {
		return report, &Error{Code: "REGISTRY_ERROR", Err: err}
	}
	if projectID != "" {
		if _, ok := reg.Projects[projectID]; !ok {
			return report, &Error{Code: "NOT_FOUND", Err: os.ErrNotExist}
		}
	}
	counts := map[string]int{}
	projects := map[string]state.Project{}
	var hosts []string
	projectIDs := make([]string, 0, len(reg.Projects))
	for id := range reg.Projects {
		projectIDs = append(projectIDs, id)
	}
	sort.Strings(projectIDs)
	corrupt := false
	for _, id := range projectIDs {
		project, e := reg.Project(id)
		if e != nil {
			corrupt = true
			report.Projects = append(report.Projects, ProjectReport{ID: id, Registry: fail("project registry is corrupt")})
			if id == projectID {
				return report, &Error{Code: "REGISTRY_ERROR", Err: e}
			}
			continue
		}
		if projectID == "" || id == projectID {
			projects[id] = project
			report.Projects = append(report.Projects, ProjectReport{ID: id, Registry: pass("project registry loaded")})
		}
		for _, i := range project.Instances {
			names := map[string]bool{}
			if i.Hostname != "" {
				names[i.Hostname] = true
			}
			for _, ep := range i.Endpoints {
				if ep.Scope == "local" && !ep.Alias && ep.InstanceID == i.ID {
					names[ep.Hostname] = true
				}
			}
			for host := range names {
				counts[host]++
				hosts = append(hosts, host)
			}
		}
	}
	cfg, cfgErr := localrouting.Load(configPath)
	if cfgErr != nil {
		report.Configuration = fail("routing configuration is invalid or unavailable")
	} else if !cfg.Routing.Enabled {
		report.Configuration = skip("routing is disabled")
	} else {
		if corrupt {
			report.Configuration = fail("cannot validate all hostnames because a project registry is corrupt")
		} else if err := routing.ValidateHosts(cfg, hosts); err != nil {
			report.Configuration = fail("routing certificates or hostnames are invalid")
		} else {
			report.Configuration = pass("local HTTP routing configuration validated")
		}
	}
	if cfgErr == nil && cfg.Legacy != nil {
		legacyHosts := []string{}
		for _, host := range hosts {
			if !strings.HasSuffix(host, ".localhost") {
				legacyHosts = append(legacyHosts, host)
			}
		}
		check := pass("legacy TLS configuration validated")
		if err := routing.ValidateHosts(*cfg.Legacy, legacyHosts); err != nil {
			check = fail("legacy TLS certificates or hostnames are invalid")
		}
		report.LegacyTLS = &check
	}
	var info proxy.Info
	var e error
	if cfgErr == nil && !cfg.Routing.Enabled {
		report.Proxy.Status = skip("routing is disabled")
	} else {
		info, e = proxy.Status(store)
		if e != nil {
			report.Proxy.Status = fail("proxy status unavailable")
		} else if info.State == "ready" {
			report.Proxy.Status = pass("ready")
		} else {
			report.Proxy.Status = fail(info.State)
		}
	}
	drift := false
	if e == nil && info.State == "ready" && cfgErr == nil {
		abs, err := filepath.Abs(configPath)
		mode := "provided"
		https := cfg.Routing.HTTPSListen
		if cfg.ExternalTLS() {
			mode = "external"
			https = ""
		}
		if err != nil || info.ConfigPath != abs || info.HTTPListen != cfg.Routing.HTTPListen || (!cfg.LocalOnly && (info.HTTPSListen != https || info.TLSMode != mode)) {
			drift = true
			report.Proxy.Status = fail("running proxy configuration path or listeners differ from configuration file")
		}
	}
	report.Proxy.HTTP = skip("routing configuration unavailable or disabled")
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if cfgErr == nil && cfg.Routing.Enabled && !drift && info.State == "ready" {
		conn, e := dial(ctx, cfg.Routing.HTTPListen)
		if e != nil {
			report.Proxy.HTTP = fail("HTTP listener unavailable")
		} else {
			conn.Close()
			report.Proxy.HTTP = pass("TCP listener reachable; forwarding identity not verified")
		}
	}
	if cfgErr == nil && info.State != "ready" {
		report.Proxy.HTTP = skip("proxy is not ready")
	}
	if drift {
		report.Proxy.HTTP = skip("running proxy configuration differs from file")
	}
	for _, projectID := range projectIDs {
		p, ok := projects[projectID]
		if !ok {
			continue
		}
		ids := make([]string, 0, len(p.Instances))
		for id, i := range p.Instances {
			if worktreeID == "" || i.WorktreeID == worktreeID {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			i := p.Instances[id]
			for _, ep := range i.Endpoints {
				if ep.Scope == "local" && !ep.Alias && ep.InstanceID == i.ID {
					i.Hostname = ep.Hostname
					i.RouteURL = "http://" + net.JoinHostPort(ep.Hostname, strconv.Itoa(ep.Port))
					break
				}
			}
			rr := RouteReport{InstanceID: id, Service: i.Service, Hostname: i.Hostname, URL: i.RouteURL, DNS: skip("no registered hostname"), TLSSystem: skip("no registered hostname"), TLSConfigured: skip("no registered hostname"), Ownership: skip("no registered hostname")}
			actual, e := store.Inspect(i)
			if e != nil {
				rr.Application = fail("application state unavailable")
			} else {
				rr.Application = pass(actual)
			}
			if cfgErr == nil && !cfg.Routing.Enabled {
				rr.DNS, rr.TLSSystem, rr.TLSConfigured, rr.Ownership = skip("routing is disabled"), skip("routing is disabled"), skip("routing is disabled"), skip("routing is disabled")
			} else if i.Hostname != "" {
				if corrupt {
					rr.Ownership = fail("hostname uniqueness cannot be verified because a project registry is corrupt")
				} else if counts[i.Hostname] != 1 {
					rr.Ownership = fail("hostname is registered more than once")
				} else if _, e := runner.VerifyProxyTarget(store, projectID, id, i.RunID); e != nil {
					rr.Ownership = fail("registered target ownership or readiness not verified")
				} else {
					rr.Ownership = pass("registered target ownership and readiness verified")
				}
				lookupCtx, stop := context.WithTimeout(ctx, time.Second)
				addrs, e := net.DefaultResolver.LookupIPAddr(lookupCtx, i.Hostname)
				stop()
				if e != nil || !loopbackAddresses(addrs) {
					rr.DNS = fail("hostname did not resolve exclusively to loopback addresses")
				} else {
					rr.DNS = pass("all resolved addresses are loopback")
				}
				rr.TLSSystem = skip("routing configuration unavailable or disabled")
				rr.TLSConfigured = rr.TLSSystem
				if cfgErr == nil && cfg.LocalOnly {
					rr.TLSSystem, rr.TLSConfigured = skip("local HTTP does not require TLS"), skip("local HTTP does not require TLS")
				} else if cfgErr == nil && cfg.Routing.Enabled && !drift {
					addr := cfg.Routing.HTTPSListen
					if cfg.ExternalTLS() {
						addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(*cfg.Routing.ExternalHTTPSPort))
					}
					if tlsProbe(ctx, addr, i.Hostname, nil) != nil {
						rr.TLSSystem = fail("TLS handshake or system certificate verification failed")
					} else {
						rr.TLSSystem = pass("TLS verified using system trust; forwarding identity not verified")
					}
					roots, e := configuredRoots(cfg)
					if e != nil {
						rr.TLSConfigured = skip("configured public certificate unavailable")
					} else if tlsProbe(ctx, addr, i.Hostname, roots) != nil {
						rr.TLSConfigured = fail("TLS handshake or configured certificate verification failed")
					} else {
						rr.TLSConfigured = pass("TLS verified using configured public certificates; forwarding identity not verified")
					}
				}
			}
			report.Routes = append(report.Routes, rr)
		}
	}
	if err := caller.Err(); err != nil {
		return report, err
	}
	return report, nil
}
