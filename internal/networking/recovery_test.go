// Tests networking recovery behavior.
package networking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRecoverAmbiguousDNSRequiresExplicitAdoption(t *testing.T) {
	actual := DNSRecord{ID: "record-1", Type: "A", Name: "*.dev.example.com", Content: "100.64.0.1"}
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
		}
		fmt.Fprint(w, `{"success":true,"result":[`)
		json.NewEncoder(w).Encode(actual)
		fmt.Fprint(w, `],"result_info":{"total_pages":1}}`)
	}))
	defer server.Close()
	s := Service{Root: filepath.Join(t.TempDir(), "config"), cloudflare: Cloudflare{BaseURL: server.URL}}
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Tailnet.CredentialRef = "token"
	plan, err := s.Plan(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if err = s.StoreCredential("token", []byte("fixture")); err != nil {
		t.Fatal(err)
	}
	pending := DNSChange{Zone: "zone", Desired: actual}
	pending.Desired.ID = ""
	if err = s.writeOwnership(ownedResources{Pending: "dns", PendingDNS: &pending}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecoverSetup(ctx); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("did not refuse ambiguity: %v", err)
	}
	if _, err = s.AdoptPendingDNS(ctx, "other-id"); err == nil {
		t.Fatal("accepted wrong record ID")
	}
	if _, err = s.AdoptPendingDNS(ctx, "record-1"); err != nil {
		t.Fatal(err)
	}
	owned, err := s.readOwnership()
	if err != nil || owned.Pending != "" || owned.DNS == nil || *owned.DNS != actual || writes != 0 {
		t.Fatalf("ownership %+v writes %d err %v", owned, writes, err)
	}
}
func TestRecoverServeOnlyTouchesVerifiedState(t *testing.T) {
	s := Service{Root: filepath.Join(t.TempDir(), "config")}
	ctx := context.Background()
	cfg := DefaultConfig()
	p, err := s.Plan(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, p); err != nil {
		t.Fatal(err)
	}
	s.tailscale = tailscaleClient{run: func(_ context.Context, args ...string) ([]byte, error) {
		if reflect.DeepEqual(args, []string{"serve", "status", "--json"}) {
			return []byte(`{"TCP":{"443":{"TCPForward":"127.0.0.1:8443"}}}`), nil
		}
		return nil, errors.New("unexpected mutation")
	}}
	if err = s.writeOwnership(ownedResources{Pending: "serve", PendingServe: "add", PendingServeTarget: managedTLSTarget}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecoverSetup(ctx); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected explicit adoption: %v", err)
	}
	if _, err = s.AdoptPendingServe(ctx); err != nil {
		t.Fatal(err)
	}
	owned, err := s.readOwnership()
	if err != nil || owned.ServeTarget != managedTLSTarget || owned.Pending != "" {
		t.Fatalf("%+v %v", owned, err)
	}
}
func TestSetupLockHonorsCancellation(t *testing.T) {
	s := Service{Root: filepath.Join(t.TempDir(), "config")}
	unlock, err := s.setupLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err = s.setupLock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock did not honor cancellation: %v", err)
	}
}

func TestRecoverOnlyDeletesOwnedChallenge(t *testing.T) {
	own := DNSRecord{ID: "own", Type: "TXT", Name: "_acme-challenge.dev.example.com", Content: "owned-value"}
	other := DNSRecord{ID: "other", Type: "TXT", Name: own.Name, Content: "unrelated"}
	var deleted string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = r.URL.Path
			fmt.Fprint(w, `{"success":true,"result":{}}`)
			return
		}
		fmt.Fprint(w, `{"success":true,"result":[`)
		json.NewEncoder(w).Encode(own)
		fmt.Fprint(w, `, `)
		json.NewEncoder(w).Encode(other)
		fmt.Fprint(w, `],"result_info":{"total_pages":1}}`)
	}))
	defer server.Close()
	s := Service{Root: filepath.Join(t.TempDir(), "config"), cloudflare: Cloudflare{BaseURL: server.URL}}
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Tailnet.CredentialRef = "token"
	p, err := s.Plan(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err = s.StoreCredential("token", []byte("fixture")); err != nil {
		t.Fatal(err)
	}
	if err = s.writeOwnership(ownedResources{Zone: "zone", Pending: "certificate", PendingChallenge: &own}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecoverSetup(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(deleted, "/own") {
		t.Fatalf("deleted unexpected resource %q", deleted)
	}
}

func TestAmbiguousFinalizationBlocksAutomaticRenewal(t *testing.T) {
	s := Service{Root: filepath.Join(t.TempDir(), "config")}
	ctx := context.Background()
	cfg := DefaultConfig()
	p, err := s.Plan(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, p); err != nil {
		t.Fatal(err)
	}
	dns := DNSRecord{ID: "owned", Type: "A", Name: "*.old.example.com", Content: "100.64.0.1"}
	if err = s.writeOwnership(ownedResources{DNS: &dns, Zone: "zone", ServeTarget: managedTLSTarget, Pending: "certificate", PendingFinalization: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RenewIfNeeded(ctx); err == nil {
		t.Fatal("automatic renewal hid ambiguous finalization")
	}
	if _, err = s.RecoverSetup(ctx); err == nil {
		t.Fatal("automatic recovery cleared ambiguous finalization")
	}
	if _, err = s.ResolvePendingFinalization(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverCompletedDNSCleanup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":true,"result":[],"result_info":{"total_pages":1}}`)
	}))
	defer server.Close()
	s := Service{Root: filepath.Join(t.TempDir(), "config"), cloudflare: Cloudflare{BaseURL: server.URL}}
	ctx := context.Background()
	cfg := DefaultConfig()
	cfg.Tailnet.CredentialRef = "token"
	p, err := s.Plan(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err = s.StoreCredential("token", []byte("fixture")); err != nil {
		t.Fatal(err)
	}
	rec := DNSRecord{ID: "owned", Type: "A", Name: "*.old.example.com", Content: "100.64.0.1"}
	if err = s.writeOwnership(ownedResources{DNS: &rec, Zone: "zone", Pending: "cleanup_dns", PendingDNS: &DNSChange{Zone: "zone", Previous: &rec}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecoverSetup(ctx); err != nil {
		t.Fatal(err)
	}
	owned, err := s.readOwnership()
	if err != nil || owned.Pending != "" || owned.DNS != nil || owned.Zone != "" {
		t.Fatalf("cleanup recovery: %+v %v", owned, err)
	}
}
