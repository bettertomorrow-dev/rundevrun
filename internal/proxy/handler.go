// Package proxy serves registered local routes.
package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

type handler struct {
	store *state.Store
	cfg   routing.Config
}
type selection struct {
	project string
	source  state.Instance
	target  state.Instance
	strip   string
}

func NewHandler(store *state.Store, cfg routing.Config) http.Handler {
	return &handler{store: store, cfg: cfg}
}

func hostname(authority string) (string, error) {
	if strings.ContainsAny(authority, "/\\@ 	\r\n") || authority == "" {
		return "", errors.New("invalid host")
	}
	if strings.Contains(authority, ":") {
		host, port, err := net.SplitHostPort(authority)
		if err != nil {
			return "", err
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("invalid port")
		}
		authority = host
	}
	if authority == "" {
		return "", errors.New("invalid host")
	}
	return strings.ToLower(authority), nil
}
func (h *handler) trusted(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, s := range h.cfg.Routing.TrustedProxies {
		if p := net.ParseIP(s); p != nil && p.Equal(ip) {
			return true
		}
		if _, n, e := net.ParseCIDR(s); e == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}
func (h *handler) selectRoute(host, path string) (selection, bool, error) {
	reg, err := h.store.Read()
	if err != nil {
		return selection{}, false, err
	}
	var out selection
	found := false
	for projectID := range reg.Projects {
		p, e := reg.Project(projectID)
		if e != nil {
			return selection{}, false, e
		}
		for _, i := range p.Instances {
			if i.Hostname != host {
				continue
			}
			if found {
				return selection{}, true, errors.New("duplicate hostname")
			}
			found = true
			out = selection{project: projectID, source: i, target: i}
		}
	}
	if !found {
		return selection{}, false, nil
	}
	if out.source.RunID == "" {
		return out, true, errors.New("inactive source")
	}
	m, err := h.store.ReadManifest(out.source.RunID)
	if err != nil || m.InstanceID != out.source.ID {
		return out, true, errors.New("invalid source manifest")
	}
	var best *state.Route
	for i := range m.Routes {
		route := &m.Routes[i]
		if route.Path == "" || route.Path[0] != '/' || strings.Contains(route.Path, "//") || strings.Contains(route.Path, "..") || route.Dependency == "" {
			return out, true, errors.New("invalid route")
		}
		prefix := strings.TrimSuffix(route.Path, "/")
		if prefix == "" {
			prefix = "/"
		}
		if path == prefix || prefix == "/" || strings.HasPrefix(path, prefix+"/") {
			if best == nil || len(route.Path) > len(best.Path) {
				best = route
			}
		}
	}
	if best != nil {
		targetID := m.EffectiveBindings[best.Dependency]
		if targetID == "" {
			return out, true, errors.New("missing route binding")
		}
		p, e := reg.Project(out.project)
		if e != nil {
			return out, true, e
		}
		target, ok := p.Instances[targetID]
		if !ok || target.Service != best.Dependency {
			return out, true, errors.New("invalid route binding")
		}
		out.target = target
		if best.StripPrefix {
			out.strip = strings.TrimSuffix(best.Path, "/")
			if out.strip == "/" {
				out.strip = ""
			}
		}
	}
	return out, true, nil
}
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	authority := r.Host
	trusted := h.trusted(r.RemoteAddr)
	if trusted && r.TLS == nil && r.Header.Get("X-Forwarded-Host") != "" {
		authority = r.Header.Get("X-Forwarded-Host")
	}
	host, err := hostname(authority)
	if err != nil {
		http.Error(w, "bad host", http.StatusBadRequest)
		return
	}
	if r.TLS != nil && (!strings.EqualFold(r.TLS.ServerName, host) || r.TLS.ServerName == "") {
		http.Error(w, "host does not match TLS name", http.StatusMisdirectedRequest)
		return
	}
	selected, known, err := h.selectRoute(host, r.URL.Path)
	if !known && err == nil {
		http.NotFound(w, r)
		return
	}
	if err != nil || selected.target.RunID == "" {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if trusted {
		if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" || proto == "http" {
			scheme = proto
		}
	}
	clientChain := ""
	if trusted {
		clientChain = r.Header.Get("X-Forwarded-For")
	}
	transport := &http.Transport{DisableKeepAlives: true, Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		current, ok, e := h.selectRoute(host, r.URL.Path)
		if e != nil || !ok || current.project != selected.project || current.source.ID != selected.source.ID || current.source.RunID != selected.source.RunID || current.target.ID != selected.target.ID || current.target.RunID != selected.target.RunID || current.strip != selected.strip {
			return nil, errors.New("route changed")
		}
		if _, e = runner.VerifyProxyTarget(h.store, selected.project, selected.source.ID, selected.source.RunID); e != nil {
			return nil, e
		}
		port, e := runner.VerifyProxyTarget(h.store, selected.project, selected.target.ID, selected.target.RunID)
		if e != nil {
			return nil, e
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	}}
	defer transport.CloseIdleConnections()
	proxy := httputil.ReverseProxy{Transport: transport, FlushInterval: -1, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}, Rewrite: func(pr *httputil.ProxyRequest) {
		pr.Out.URL.Scheme = "http"
		pr.Out.URL.Host = "127.0.0.1:1"
		pr.Out.Host = authority
		if selected.strip != "" {
			pr.Out.URL.Path = strings.TrimPrefix(pr.Out.URL.Path, selected.strip)
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path = "/"
			}
			pr.Out.URL.RawPath = ""
		}
		for name := range pr.Out.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-forwarded-") || strings.EqualFold(name, "Forwarded") || strings.EqualFold(name, "X-Real-IP") {
				pr.Out.Header.Del(name)
			}
		}
		pr.SetXForwarded()
		pr.Out.Header.Set("X-Forwarded-Host", authority)
		pr.Out.Header.Set("X-Forwarded-Proto", scheme)
		if clientChain != "" && validForwardedFor(clientChain) {
			pr.Out.Header.Set("X-Forwarded-For", clientChain+", "+remoteIP(r.RemoteAddr))
		}
	}}
	proxy.ServeHTTP(w, r)
}
func remoteIP(remote string) string { host, _, _ := net.SplitHostPort(remote); return host }
func validForwardedFor(value string) bool {
	for _, item := range strings.Split(value, ",") {
		if net.ParseIP(strings.TrimSpace(item)) == nil {
			return false
		}
	}
	return true
}
