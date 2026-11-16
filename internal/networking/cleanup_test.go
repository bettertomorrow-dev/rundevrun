// Tests networking cleanup behavior.
package networking

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCleanupRejectsEditsAndAllowsNewDomain(t *testing.T) {
	old := DNSRecord{ID: "owned", Type: "A", Name: "*.old.example.com", Content: "100.64.0.1"}
	unrelated := DNSRecord{ID: "other", Type: "A", Name: "npm.example.com", Content: "100.64.0.9"}
	records := []DNSRecord{old, unrelated}
	deletes := 0
	cfserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
			if r.URL.Path != "/zones/zone/dns_records/owned" {
				t.Errorf("unexpected deletion %s", r.URL.Path)
			}
			records = records[1:]
			fmt.Fprint(w, `{"success":true,"result":{}}`)
			return
		}
		fmt.Fprint(w, `{"success":true,"result":`)
		json.NewEncoder(w).Encode(records)
		fmt.Fprint(w, `,"result_info":{"total_pages":1}}`)
	}))
	defer cfserver.Close()
	var acme *httptest.Server
	acme = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"newNonce":%q,"newAccount":%q,"newOrder":%q,"meta":{"termsOfService":"https://example.com/terms"}}`, acme.URL+"/nonce", acme.URL+"/account", acme.URL+"/order")
	}))
	defer acme.Close()
	serve := true
	changes := 0
	s := Service{Root: filepath.Join(t.TempDir(), "config"), cloudflare: Cloudflare{BaseURL: cfserver.URL}, issuer: certificateIssuer{directory: acme.URL}}
	s.tailscale = tailscaleClient{run: func(_ context.Context, args ...string) ([]byte, error) {
		if reflect.DeepEqual(args, []string{"serve", "status", "--json"}) {
			if serve {
				return []byte(`{"TCP":{"443":{"TCPForward":"127.0.0.1:8443"},"80":{"HTTP":true}}}`), nil
			}
			return []byte(`{"TCP":{"80":{"HTTP":true}}}`), nil
		}
		if reflect.DeepEqual(args, []string{"status", "--json"}) {
			return []byte(`{"BackendState":"Running","Self":{"TailscaleIPs":["100.64.0.1"]}}`), nil
		}
		if reflect.DeepEqual(args, []string{"serve", "--bg", "--tcp=443", "off"}) {
			serve = false
			changes++
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected command %v", args)
	}}
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Tailnet = Tailnet{Domain: "new.example.com", ZoneID: "zone", CredentialRef: "token", Email: "owner@example.com"}
	cfg.Projects["project"] = Project{Tailnet: true}
	p, err := s.Plan(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err = s.StoreCredential("token", []byte("fixture-token")); err != nil {
		t.Fatal(err)
	}
	if err = s.writeOwnership(ownedResources{DNS: &old, Zone: "zone", ServeTarget: managedTLSTarget}); err != nil {
		t.Fatal(err)
	}
	certDir := filepath.Join(s.Root, "certificates")
	if err = os.Mkdir(certDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(certDir, "current.pem"), []byte("old certificate fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	records[0].Content = "100.64.0.42"
	if _, err = s.PlanCleanup(ctx); err == nil {
		t.Fatal("accepted externally changed DNS")
	}
	if changes != 0 || deletes != 0 {
		t.Fatal("changed resources while preview rejected")
	}
	records[0] = old
	preview, err := s.PlanCleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Actions) != 2 || preview.DNS == nil || preview.DNS.ID != "owned" {
		t.Fatalf("preview %+v", preview)
	}
	records[0].Content = "100.64.0.42"
	if _, err = s.ApplyCleanup(ctx, preview); err == nil {
		t.Fatal("accepted changed resource after preview")
	}
	if changes != 0 || deletes != 0 {
		t.Fatal("changed resources after rejected apply")
	}
	records[0] = old
	if _, err = s.ApplyCleanup(ctx, preview); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 || changes != 1 || len(records) != 1 || records[0] != unrelated {
		t.Fatalf("unrelated resource changed: %+v", records)
	}
	if _, err = os.Stat(filepath.Join(s.Root, "credentials", "token")); err != nil {
		t.Fatal("credential removed")
	}
	if _, err = os.Stat(filepath.Join(certDir, "current.pem")); err != nil {
		t.Fatal("certificate removed")
	}
	plan, err := s.PlanSetup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if plan.DNS.Desired.Name != "*.new.example.com" || plan.DNSAction != "create" {
		t.Fatalf("new domain unavailable: %+v", plan)
	}
}
