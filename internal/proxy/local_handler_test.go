// Tests proxy local handler behavior.
package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestLocalAndTailnetRoutesAreScoped(t *testing.T) {
	store, err := state.New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	id := "p/w/web"
	err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject("p", state.Project{
			ID: "p", Name: "demo", Root: "/tmp/demo", Worktrees: []state.Worktree{{ID: "w", Root: "/tmp/demo"}},
			Instances: map[string]state.Instance{id: {ID: id, WorktreeID: "w", Service: "web", State: "stopped", Hostname: "web-main-demo.localhost", Endpoints: []state.Endpoint{
				{Scope: "local", Hostname: "web-main-demo.localhost", Port: 8080, InstanceID: id},
				{Scope: "tailnet", Hostname: "web-main-demo.dev.example.com", Port: 443, InstanceID: id},
			}}},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := routing.Config{LocalOnly: true, TailnetDomain: "dev.example.com", LocalProjects: map[string]routing.LocalProject{"p": {Tailnet: true}}}
	check := func(host, sni string, secure bool, want int) {
		t.Helper()
		r := httptest.NewRequest("GET", "http://"+host+"/", nil)
		r.Host = host
		if secure {
			r.TLS = &tls.ConnectionState{ServerName: sni}
		}
		w := httptest.NewRecorder()
		NewHandler(store, cfg).ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("host=%s sni=%s: got %d want %d", host, sni, w.Code, want)
		}
	}
	check("web-main-demo.localhost", "", false, 503)
	check("web-main-demo.dev.example.com", "", false, 404)
	check("web-main-demo.dev.example.com", "web-main-demo.dev.example.com", true, 503)
	check("web-main-demo.localhost", "web-main-demo.localhost", true, 404)
	check("web-main-demo.dev.example.com", "other.dev.example.com", true, 421)
	cfg.LocalProjects["p"] = routing.LocalProject{}
	check("web-main-demo.dev.example.com", "web-main-demo.dev.example.com", true, 404)
}

func TestLegacyAndLocalHostsShareCandidate(t *testing.T) {
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"*.dev.example.com"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.toml")
	config := "version = 1\n[routing]\nenabled = true\ndomain = \"dev.example.com\"\n[[routing.tls.certificates]]\ncert_file = \"cert.pem\"\nkey_file = \"key.pem\"\n"
	if err = os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := state.New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	id := "p/w/web"
	err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject("p", state.Project{ID: "p", Name: "demo", Root: "/tmp/demo", Worktrees: []state.Worktree{{ID: "w", Root: "/tmp/demo"}}, Instances: map[string]state.Instance{id: {ID: id, WorktreeID: "w", Service: "web", State: "stopped", Hostname: "web-main-demo.dev.example.com", RouteURL: "https://web-main-demo.dev.example.com:8443", Endpoints: []state.Endpoint{{Scope: "local", Hostname: "web-main-demo.localhost", Port: 8080, InstanceID: id}}}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := buildCandidate(store, configPath)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.legacyTLS == nil {
		t.Fatal("legacy TLS certificate was dropped")
	}
	check := func(host string, secure bool, want int) {
		t.Helper()
		req := httptest.NewRequest("GET", "http://"+host+"/", nil)
		req.Host = host
		if secure {
			req.TLS = &tls.ConnectionState{ServerName: host}
		}
		w := httptest.NewRecorder()
		candidate.handler.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s got %d want %d", host, w.Code, want)
		}
	}
	check("web-main-demo.localhost", false, 503)
	check("web-main-demo.localhost", true, 404)
	check("web-main-demo.dev.example.com", true, 503)
}
