// Tests networking cloudflare behavior.
package networking

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWildcardReconciliation(t *testing.T) {
	var records []DNSRecord
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("wrong authorization")
		}
		switch r.Method {
		case http.MethodGet:
			fmt.Fprint(w, `{"success":true,"result":`)
			json.NewEncoder(w).Encode(records)
			fmt.Fprint(w, `,"result_info":{"total_pages":1}}`)
		case http.MethodPost, http.MethodPut:
			writes++
			var record DNSRecord
			if err := json.NewDecoder(r.Body).Decode(&record); err != nil {
				t.Error(err)
			}
			record.ID = "owned"
			records = []DNSRecord{record}
			fmt.Fprint(w, `{"success":true,"result":`)
			json.NewEncoder(w).Encode(record)
			fmt.Fprint(w, `}`)
		}
	}))
	defer server.Close()
	cf := Cloudflare{BaseURL: server.URL, Token: "fixture-token"}
	ctx := context.Background()
	p, err := cf.PlanWildcard(ctx, "zone", "dev.example.com", "100.64.0.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatal("planning mutated DNS")
	}
	owned, err := cf.ApplyWildcard(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	p, err = cf.PlanWildcard(ctx, "zone", "dev.example.com", "100.64.0.1", &owned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cf.ApplyWildcard(ctx, p); err != nil || writes != 1 {
		t.Fatalf("idempotence: %v writes %d", err, writes)
	}
	records[0].Content = "100.64.0.9"
	if _, err = cf.PlanWildcard(ctx, "zone", "dev.example.com", "100.64.0.1", &owned); err == nil {
		t.Fatal("accepted external edit")
	}
	if _, err = cf.ApplyWildcard(ctx, p); err == nil || writes != 1 {
		t.Fatal("mutated a changed record")
	}
	if _, err = cf.PlanWildcard(ctx, "zone", "dev.example.com", "100.64.0.1", nil); err == nil {
		t.Fatal("adopted an unmanaged record")
	}
}

func TestCredentialsArePrivateAndNeverReplaced(t *testing.T) {
	root := filepath.Join(t.TempDir(), "network")
	s := Service{Root: root}
	if err := s.StoreCredential("cf", []byte("fixture-token")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(root, "credentials", "cf"))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("credential mode: %v %v", st, err)
	}
	if err := s.StoreCredential("cf", []byte("another")); err == nil {
		t.Fatal("overwrote credential")
	}
	if _, err := s.credential("../cf"); err == nil {
		t.Fatal("allowed traversal")
	}
	if err := os.Chmod(filepath.Join(root, "credentials", "cf"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.credential("cf"); err == nil || strings.Contains(err.Error(), "fixture-token") {
		t.Fatal("accepted or exposed credential")
	}
}

func TestRevokedCredentialDoesNotLeakOrMutate(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
		}
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"success":false,"errors":[{"message":"fixture-secret"}]}`)
	}))
	defer server.Close()
	cf := Cloudflare{BaseURL: server.URL, Token: "fixture-secret"}
	_, err := cf.PlanWildcard(context.Background(), "zone", "dev.example.com", "100.64.0.1", nil)
	if err == nil || strings.Contains(err.Error(), "fixture-secret") || writes != 0 {
		t.Fatalf("credential failure handling: %v writes=%d", err, writes)
	}
}
