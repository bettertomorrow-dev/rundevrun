// Tests routing external behavior.
package routing

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExternalPublicCertificates(t *testing.T) {
	now := time.Now()
	pair := writePair(t, []string{"*.dev.example.com"}, now.Add(-time.Hour), now.Add(time.Hour), false)
	if err := os.Remove(pair.KeyFile); err != nil {
		t.Fatal(err)
	}
	port := 443
	c := Config{Routing: Settings{HTTPSListen: "127.0.0.1:8443", ExternalHTTPSPort: &port, TrustedProxies: []string{"127.0.0.1"}, TLS: TLSSettings{Mode: "external", Certificates: []Certificate{{CertFile: pair.CertFile}}}}}
	if err := ValidateHosts(c, []string{"app.dev.example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTLS(c, nil); err == nil {
		t.Fatal("external mode returned TLS server config")
	}
	certBytes, err := os.ReadFile(pair.CertFile)
	if err != nil {
		t.Fatal(err)
	}
	mixed := filepath.Join(t.TempDir(), "mixed.pem")
	if err = os.WriteFile(mixed, append(certBytes, []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	c.Routing.TLS.Certificates[0].CertFile = mixed
	if err = ValidateHosts(c, []string{"app.dev.example.com"}); err == nil || strings.Contains(err.Error(), "AAAA") {
		t.Fatalf("private key in public chain: %v", err)
	}
	c.Routing.TLS.Certificates[0].CertFile = pair.CertFile

	for _, host := range []string{"nested.app.dev.example.com", "uncovered.example.com"} {
		if err := ValidateHosts(c, []string{host}); err == nil {
			t.Fatalf("accepted %s", host)
		}
	}
	expired := writePair(t, []string{"*.dev.example.com"}, now.Add(-2*time.Hour), now.Add(-time.Hour), false)
	c.Routing.TLS.Certificates[0].CertFile = expired.CertFile
	var re *Error
	if err := ValidateHosts(c, []string{"app.dev.example.com"}); !errors.As(err, &re) || re.Code != "TLS_INVALID" {
		t.Fatalf("expired: %v", err)
	}
}
func TestExternalConfigValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	base := "version=1\n[routing]\nenabled=true\ndomain='dev.example.com'\n%s\n[routing.tls]\nmode='external'\n[[routing.tls.certificates]]\ncert_file='cert.pem'\n%s\n"
	for _, v := range []struct{ fields, key string }{{"external_https_port=443", ""}, {"trusted_proxies=['127.0.0.1']", ""}, {"trusted_proxies=['10.0.0.1']\nexternal_https_port=443", ""}, {"trusted_proxies=['127.0.0.1']\nexternal_https_port=443", "key_file='secret.pem'"}} {
		if err := os.WriteFile(path, []byte(fmt.Sprintf(base, v.fields, v.key)), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted %q %q", v.fields, v.key)
		}
	}
	good := fmt.Sprintf(base, "trusted_proxies=['127.0.0.1']\nexternal_https_port=443", "")
	if err := os.WriteFile(path, []byte(good), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || !cfg.ExternalTLS() || cfg.Routing.TLS.Certificates[0].CertFile != filepath.Join(dir, "cert.pem") {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	if url, err := cfg.URL("app.dev.example.com"); err != nil || url != "https://app.dev.example.com" {
		t.Fatalf("url %q %v", url, err)
	}
	if err = os.WriteFile(path, []byte(strings.Replace(good, "mode='external'", "mode='unknown'", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Load(path); err == nil {
		t.Fatal("unknown mode accepted")
	}
}
