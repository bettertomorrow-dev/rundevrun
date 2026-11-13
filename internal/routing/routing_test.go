package routing

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigAndHostname(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "config.toml")
	c, err := Load(path)
	if err != nil || c.Routing.Enabled {
		t.Fatalf("missing config: %+v %v", c, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Load created config directory")
	}
	host, err := c.Hostname("Console", "Feature/Test", "MillDrum", "dev.example.com")
	if err != nil || host != "console-feature-test-milldrum.dev.example.com" {
		t.Fatalf("host=%q err=%v", host, err)
	}
	url, err := c.URL(host)
	if err != nil || url != "https://"+host+":8443" {
		t.Fatalf("url=%q err=%v", url, err)
	}
	p := 443
	c.Routing.ExternalHTTPSPort = &p
	url, err = c.URL(host)
	if err != nil || url != "https://"+host {
		t.Fatalf("external URL=%q %v", url, err)
	}
	for _, worktree := range []string{"", "***", strings.Repeat("a", 64)} {
		if _, err := c.Hostname("api", worktree, "project", "example.com"); err == nil || !strings.Contains(err.Error(), "alias") {
			t.Fatalf("invalid worktree %q: %v", worktree, err)
		}
	}
	c.Routing.Hostname = "{service}.{worktree}.{domain}"
	if _, err := c.Hostname("api", "main", "p", "example.com"); err == nil {
		t.Fatal("nested template accepted")
	}
	c.Routing.Hostname = "{unknown}.{domain}"
	if _, err := c.Hostname("api", "main", "p", "example.com"); err == nil {
		t.Fatal("unknown template token accepted")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"[routing]\nenabled = true", "version=2", "version=1\n[routing]\nunknown=true", "version=1\n[routing]\nenabled=true", "version=1\n[routing]\nhttp_listen='0.0.0.0:80'", "version=1\n[routing]\ntrusted_proxies=['10.0.0.1']"} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted %q", content)
		}
	}
	valid := "version=1\n[routing]\nenabled=true\ndomain='EXAMPLE.com'\n[[routing.tls.certificates]]\ncert_file='cert.pem'\nkey_file='key.pem'\n"
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Routing.Domain != "example.com" || c.Routing.TLS.Certificates[0].KeyFile != filepath.Join(filepath.Dir(path), "key.pem") {
		t.Fatalf("config: %+v", c)
	}
}

func writePair(t *testing.T, names []string, before, after time.Time, mismatch bool) Certificate {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "unused"}, DNSNames: names, NotBefore: before, NotAfter: after, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if mismatch {
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return Certificate{CertFile: certFile, KeyFile: keyFile}
}
func TestTLS(t *testing.T) {
	now := time.Now()
	pair := writePair(t, []string{"*.dev.example.com"}, now.Add(-time.Hour), now.Add(time.Hour), false)
	c := Config{Routing: Settings{TLS: TLSSettings{Certificates: []Certificate{pair}}}}
	candidate, err := LoadTLS(c, []string{"api.dev.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = candidate.GetCertificate(&tls.ClientHelloInfo{ServerName: "api.dev.example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"nested.api.dev.example.com", "other.example.com", ""} {
		if _, err = candidate.GetCertificate(&tls.ClientHelloInfo{ServerName: host}); err == nil {
			t.Fatalf("accepted %q", host)
		}
	}
	if _, err = LoadTLS(c, []string{"nested.api.dev.example.com"}); err == nil {
		t.Fatal("wildcard covered two labels")
	}
	for _, cert := range []Certificate{writePair(t, []string{"a.example.com"}, now.Add(-2*time.Hour), now.Add(-time.Hour), false), writePair(t, []string{"a.example.com"}, now.Add(time.Hour), now.Add(2*time.Hour), false), writePair(t, []string{"a.example.com"}, now.Add(-time.Hour), now.Add(time.Hour), true)} {
		c.Routing.TLS.Certificates = []Certificate{cert}
		_, err = LoadTLS(c, []string{"a.example.com"})
		var te *Error
		if !errors.As(err, &te) || te.Code != "TLS_INVALID" {
			t.Fatalf("expected TLS_INVALID: %v", err)
		}
		if strings.Contains(err.Error(), "PRIVATE KEY") {
			t.Fatal("private key leaked")
		}
	}
}
