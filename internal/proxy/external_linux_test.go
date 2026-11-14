//go:build linux

package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExternalHandlerRequiresTrustedHTTPS(t *testing.T) {
	store, cfg, _ := fixture(t)
	cfg.Routing.TLS.Mode = "external"
	cfg.Routing.TrustedProxies = []string{"127.0.0.1"}
	h := NewHandler(store, cfg)
	for _, tc := range []struct {
		remote, proto string
		want          int
	}{{"192.0.2.1:1234", "https", 403}, {"127.0.0.1:1234", "http", 403}, {"127.0.0.1:1234", "", 403}, {"127.0.0.1:1234", "https", 200}} {
		req := httptest.NewRequest("GET", "http://web.example.com/", nil)
		req.RemoteAddr = tc.remote
		if tc.proto != "" {
			req.Header.Set("X-Forwarded-Proto", tc.proto)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("remote=%s proto=%s: %d %s", tc.remote, tc.proto, w.Code, w.Body.String())
		}
	}
	req := httptest.NewRequest("GET", "http://web.example.com/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Add("X-Forwarded-Proto", "https")
	req.Header.Add("X-Forwarded-Proto", "http")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("ambiguous forwarded proto accepted")
	}
}
func externalConfigFile(t *testing.T, dir, httpAddr, httpsAddr, cert string) string {
	t.Helper()
	p := filepath.Join(dir, "config.toml")
	text := fmt.Sprintf("version=1\n[routing]\nenabled=true\ndomain='example.com'\nhttp_listen=%q\nhttps_listen=%q\nexternal_https_port=443\ntrusted_proxies=['127.0.0.1']\n[routing.tls]\nmode='external'\n[[routing.tls.certificates]]\ncert_file=%q\n", httpAddr, httpsAddr, cert)
	if err := os.WriteFile(p, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestExternalDaemonAndReload(t *testing.T) {
	store, _, _ := fixture(t)
	dir := t.TempDir()
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	httpAddr := freeAddress(t)
	httpsAddr := occupied.Addr().String()
	cert, key := certPair(t, dir, 71)
	if err = os.Remove(key); err != nil {
		t.Fatal(err)
	}
	path := externalConfigFile(t, dir, httpAddr, httpsAddr, cert)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = Stop(context.Background(), store) })
	if err = Ensure(ctx, store, exe, path); err != nil {
		t.Fatal(err)
	}
	request := func(proto string) int {
		t.Helper()
		req, e := http.NewRequest("GET", "http://"+httpAddr+"/", nil)
		if e != nil {
			t.Fatal(e)
		}
		req.Host = "web.example.com"
		if proto != "" {
			req.Header.Set("X-Forwarded-Proto", proto)
		}
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		return res.StatusCode
	}
	if got := request(""); got != 403 {
		t.Fatalf("plain request %d", got)
	}
	if got := request("https"); got != 200 {
		t.Fatalf("trusted request %d", got)
	}
	nextCert, nextKey := certPair(t, dir, 72)
	_ = os.Remove(nextKey)
	externalConfigFile(t, dir, httpAddr, httpsAddr, nextCert)
	if err = Reload(ctx, store); err != nil {
		t.Fatalf("valid external reload: %v", err)
	}
	externalConfigFile(t, dir, httpAddr, httpsAddr, filepath.Join(dir, "missing.pem"))
	var ce *Error
	if err = Reload(ctx, store); !errors.As(err, &ce) || ce.Code != "TLS_INVALID" {
		t.Fatalf("invalid external reload: %v", err)
	}
	if got := request("https"); got != 200 {
		t.Fatalf("old config lost after failed reload: %d", got)
	}
	providedCert, providedKey := certPair(t, dir, 73)
	configFile(t, dir, httpAddr, httpsAddr, providedCert, providedKey)
	if err = Reload(ctx, store); !errors.As(err, &ce) || ce.Code != "CONFIG_INVALID" {
		t.Fatalf("mode transition accepted: %v", err)
	}
	if got := request("https"); got != 200 {
		t.Fatalf("old mode lost after failed reload: %d", got)
	}
}
