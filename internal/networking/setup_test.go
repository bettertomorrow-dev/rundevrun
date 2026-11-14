package networking

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSetupPlanNoMutationAndRejectsForgedPlan(t *testing.T) {
	writes := 0
	cf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
		}
		fmt.Fprint(w, `{"success":true,"result":[],"result_info":{"total_pages":1}}`)
	}))
	defer cf.Close()
	var acme *httptest.Server
	acme = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"newNonce":%q,"newAccount":%q,"newOrder":%q,"meta":{"termsOfService":"https://example.com/terms"}}`, acme.URL+"/nonce", acme.URL+"/account", acme.URL+"/order")
	}))
	defer acme.Close()
	s := Service{Root: filepath.Join(t.TempDir(), "config"), cloudflare: Cloudflare{BaseURL: cf.URL}, issuer: certificateIssuer{directory: acme.URL}}
	s.tailscale = tailscaleClient{run: func(_ context.Context, args ...string) ([]byte, error) {
		if reflect.DeepEqual(args, []string{"serve", "status", "--json"}) {
			return []byte(`{}`), nil
		}
		if reflect.DeepEqual(args, []string{"status", "--json"}) {
			return []byte(`{"BackendState":"Running","Self":{"TailscaleIPs":["100.64.0.1"]}}`), nil
		}
		writes++
		return nil, nil
	}}
	ctx := context.Background()
	desired := DefaultConfig()
	desired.Tailnet = Tailnet{Domain: "dev.example.com", ZoneID: "zone", CredentialRef: "token", Email: "owner@example.com"}
	desired.Projects["project"] = Project{Tailnet: true}
	configPlan, err := s.Plan(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, configPlan); err != nil {
		t.Fatal(err)
	}
	if err = s.StoreCredential("token", []byte("fixture-token")); err != nil {
		t.Fatal(err)
	}
	plan, err := s.PlanSetup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatal("preview mutated provider")
	}
	if plan.DNSAction != "create" || plan.ServeAction != "add" || plan.TermsURL != "https://example.com/terms" {
		t.Fatalf("plan %+v", plan)
	}
	bad := plan
	bad.DNS.Desired.Content = "100.64.0.42"
	bad.TermsAccepted = true
	if _, err = s.ApplySetup(ctx, bad); err == nil {
		t.Fatal("accepted forged DNS target")
	}
	if writes != 0 {
		t.Fatal("forged plan mutated provider")
	}
	if _, err = s.ApplySetup(ctx, plan); err == nil || !strings.Contains(err.Error(), "terms") {
		t.Fatalf("did not require explicit terms: %v", err)
	}
	if writes != 0 {
		t.Fatal("unapproved terms mutated provider")
	}
}
