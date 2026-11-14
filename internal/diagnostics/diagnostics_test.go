package diagnostics

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestLoopbackRestrictions(t *testing.T) {
	if !loopbackAddresses([]net.IPAddr{{IP: net.ParseIP("127.0.0.1")}, {IP: net.ParseIP("::1")}}) {
		t.Fatal("loopback rejected")
	}
	if loopbackAddresses(nil) || loopbackAddresses([]net.IPAddr{{IP: net.ParseIP("127.0.0.1")}, {IP: net.ParseIP("192.0.2.1")}}) {
		t.Fatal("non-loopback accepted")
	}
	if _, err := dial(context.Background(), "192.0.2.1:443"); err == nil {
		t.Fatal("non-loopback dial accepted")
	}
}
func TestTLSVerification(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	if err := tlsProbe(context.Background(), server.Listener.Addr().String(), "example.invalid", roots); err == nil {
		t.Fatal("wrong hostname accepted")
	}
	if err := tlsProbe(context.Background(), server.Listener.Addr().String(), "example.com", roots); err != nil {
		t.Fatalf("valid certificate: %v", err)
	}
	if err := tlsProbe(context.Background(), server.Listener.Addr().String(), "example.com", x509.NewCertPool()); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
}
func TestRunDoesNotCreateState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	_, err := Run(context.Background(), &state.Store{Root: root}, filepath.Join(root, "config.toml"), "missing", "")
	if err == nil {
		t.Fatal("missing project accepted")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("state directory created: %v", err)
	}
}

func TestGlobalReadOnlyAndInvalidTLS(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	registry := `{"schema":1,"revision":1,"projects":{"good":{"id":"good","name":"Good","root":"/tmp","worktrees":[{"id":"w","root":"/tmp"}],"instances":{"good/w/web":{"id":"good/w/web","service":"web","worktree_id":"w","state":"stopped"}}},"bad":{"id":"bad"}}}`
	path := filepath.Join(root, "registry.json")
	if err := os.WriteFile(path, []byte(registry), 0600); err != nil {
		t.Fatal(err)
	}
	store := &state.Store{Root: root}
	config := filepath.Join(root, "config.toml")
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	report, err := Run(context.Background(), store, config, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Projects) != 2 || report.Projects[0].ID != "bad" || report.Projects[0].Registry.Status != "fail" || len(report.Routes) != 1 || report.Routes[0].Application.Message != "stopped" {
		t.Fatalf("unexpected global report: %+v", report)
	}
	after, _ := os.ReadDir(root)
	again, _ := os.ReadFile(path)
	if len(before) != len(after) || !bytes.Equal(data, again) {
		t.Fatal("diagnostics changed state")
	}
	if _, err := Run(context.Background(), store, config, "unknown", ""); err == nil {
		t.Fatal("missing project accepted")
	} else if e, ok := err.(*Error); !ok || e.Code != "NOT_FOUND" {
		t.Fatalf("unexpected error: %v", err)
	}
	// Remove the corrupt project so TLS validation can reach the invalid key.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["projects"] = json.RawMessage(`{"good":{"id":"good","name":"Good","root":"/tmp","worktrees":[{"id":"w","root":"/tmp"}],"instances":{"good/w/web":{"id":"good/w/web","service":"web","worktree_id":"w","state":"stopped","hostname":"pinned.example.invalid"}}}}`)
	data, _ = json.Marshal(raw)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	report, err = Run(context.Background(), store, config, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if report.Configuration.Status != "skip" || report.Proxy.Status.Status != "skip" || report.Proxy.HTTP.Status != "skip" || len(report.Projects) != 1 || report.Projects[0].Registry.Status != "pass" || len(report.Routes) != 1 || report.Routes[0].Application.Status != "pass" || report.Routes[0].Application.Message != "stopped" || report.Routes[0].DNS.Status != "skip" || report.Routes[0].Ownership.Status != "skip" {
		t.Fatalf("disabled routing report: %+v", report)
	}
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	certPath := filepath.Join(root, "missing.crt")
	keyPath := filepath.Join(root, "missing.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("invalid key"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("version = 1\n[routing]\nenabled = true\ndomain = \"example.test\"\n[routing.tls]\nmode = \"provided\"\n[[routing.tls.certificates]]\ncert_file = \"%s\"\nkey_file = \"%s\"\n", filepath.Join(root, "missing.crt"), filepath.Join(root, "missing.key"))
	if err := os.WriteFile(config, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	report, err = Run(context.Background(), store, config, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if report.Configuration.Status != "fail" || len(report.Routes) != 1 || report.Routes[0].Application.Message != "stopped" {
		t.Fatalf("invalid TLS report: %+v", report)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, store, config, "", ""); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
}
