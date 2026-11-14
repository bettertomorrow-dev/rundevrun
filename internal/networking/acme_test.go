package networking

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestACMEIssuanceAndChallengeCleanup(t *testing.T) {
	now := time.Now()
	clockNow := now
	var cfReads int
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(90 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	var challenge DNSRecord
	var wildcard *DNSRecord
	deleted := false
	failChallenge := false
	cfserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var incoming DNSRecord
			json.NewDecoder(r.Body).Decode(&incoming)
			if incoming.Type == "A" {
				incoming.ID = "owned"
				wildcard = &incoming
			} else {
				incoming.ID = "challenge"
				challenge = incoming
			}
			fmt.Fprint(w, `{"success":true,"result":`)
			json.NewEncoder(w).Encode(incoming)
			fmt.Fprint(w, `}`)
		case http.MethodGet:
			cfReads++
			fmt.Fprint(w, `{"success":true,"result":[`)
			if strings.HasPrefix(r.URL.Query().Get("name.exact"), "*.") {
				if wildcard != nil {
					json.NewEncoder(w).Encode(wildcard)
				}
			} else {
				json.NewEncoder(w).Encode(challenge)
				fmt.Fprint(w, `,`)
				json.NewEncoder(w).Encode(DNSRecord{ID: "unrelated", Type: "TXT", Name: challenge.Name, Content: "do-not-delete"})
			}
			fmt.Fprint(w, `],"result_info":{"total_pages":1}}`)
		case http.MethodDelete:
			if !strings.HasSuffix(r.URL.Path, "/challenge") {
				t.Errorf("deleted unrelated record: %s", r.URL.Path)
			}
			deleted = true
			fmt.Fprint(w, `{"success":true,"result":{}}`)
		}
	}))
	defer cfserver.Close()
	var server *httptest.Server
	authorized := false
	var certPEM []byte
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "fixture-nonce")
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/directory" {
			fmt.Fprintf(w, `{"newNonce":%q,"newAccount":%q,"newOrder":%q,"meta":{"termsOfService":"https://example.com/terms"}}`, server.URL+"/nonce", server.URL+"/account", server.URL+"/new-order")
			return
		}
		if r.URL.Path == "/nonce" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var jws struct {
			Payload string `json:"payload"`
		}
		json.NewDecoder(r.Body).Decode(&jws)
		payload, _ := base64.RawURLEncoding.DecodeString(jws.Payload)
		switch r.URL.Path {
		case "/account":
			w.Header().Set("Location", server.URL+"/account/1")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"status":"valid"}`)
		case "/new-order":
			certPEM = nil
			w.Header().Set("Location", server.URL+"/order/1")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"status":"pending","authorizations":[%q],"finalize":%q}`, server.URL+"/auth/1", server.URL+"/finalize/1")
		case "/auth/1":
			status := "pending"
			if authorized {
				status = "valid"
			}
			fmt.Fprintf(w, `{"status":%q,"identifier":{"type":"dns","value":"dev.example.com"},"wildcard":true,"challenges":[{"type":"dns-01","url":%q,"status":"pending","token":"test-token"}]}`, status, server.URL+"/challenge/1")
		case "/challenge/1":
			if failChallenge {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"type":"urn:ietf:params:acme:error:serverInternal","detail":"temporary failure"}`)
				return
			}
			authorized = true
			fmt.Fprintf(w, `{"type":"dns-01","url":%q,"status":"processing","token":"test-token"}`, server.URL+"/challenge/1")
		case "/order/1":
			status := "ready"
			if len(certPEM) > 0 {
				status = "valid"
			}
			fmt.Fprintf(w, `{"status":%q,"authorizations":[%q],"finalize":%q,"certificate":%q}`, status, server.URL+"/auth/1", server.URL+"/finalize/1", server.URL+"/cert/1")
		case "/finalize/1":
			var p struct {
				CSR string `json:"csr"`
			}
			json.Unmarshal(payload, &p)
			der, e := base64.RawURLEncoding.DecodeString(p.CSR)
			if e != nil {
				t.Error(e)
			}
			csr, e := x509.ParseCertificateRequest(der)
			if e != nil {
				t.Error(e)
			}
			leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: csr.DNSNames, NotBefore: clockNow.Add(-time.Hour), NotAfter: clockNow.Add(60 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
			cert, e := x509.CreateCertificate(rand.Reader, leaf, ca, csr.PublicKey, caKey)
			if e != nil {
				t.Error(e)
			}
			certPEM = append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
			fmt.Fprintf(w, `{"status":"valid","certificate":%q}`, server.URL+"/cert/1")
		case "/cert/1":
			w.Header().Set("Content-Type", "application/pem-certificate-chain")
			w.Write(certPEM)
		default:
			t.Errorf("unexpected ACME path %s payload %s", r.URL.Path, strings.TrimSpace(string(payload)))
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	issuer := certificateIssuer{directory: server.URL + "/directory", roots: roots, now: func() time.Time { return clockNow }, resolveTXT: func(context.Context, string) ([]string, error) { return []string{challenge.Content}, nil }}
	s := Service{Root: filepath.Join(t.TempDir(), "config")}
	cf := Cloudflare{BaseURL: cfserver.URL, Token: "fixture-token"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cert, err := issuer.issueCertificate(ctx, s, cf, Tailnet{Domain: "dev.example.com", ZoneID: "zone", Email: "owner@example.com"}, "https://example.com/terms")
	if err != nil {
		t.Fatal(err)
	}
	if !deleted || cert.Leaf.DNSNames[0] != "*.dev.example.com" {
		t.Fatalf("cleanup %v certificate %+v", deleted, cert.Leaf)
	}
	if _, err = s.managedCertificate("dev.example.com", roots, now); err != nil {
		t.Fatalf("published certificate: %v", err)
	}
	// Exercise a complete explicit setup in a fresh configuration as well.
	s = Service{Root: filepath.Join(t.TempDir(), "fresh"), issuer: issuer, cloudflare: Cloudflare{BaseURL: cfserver.URL}}
	serveStatus := `{}`
	s.tailscale = tailscaleClient{run: func(_ context.Context, args ...string) ([]byte, error) {
		if reflect.DeepEqual(args, []string{"serve", "status", "--json"}) {
			return []byte(serveStatus), nil
		}
		if reflect.DeepEqual(args, []string{"status", "--json"}) {
			return []byte(`{"BackendState":"Running","Self":{"TailscaleIPs":["100.64.0.1"]}}`), nil
		}
		if reflect.DeepEqual(args, []string{"serve", "--bg", "--tcp=443", "tcp://127.0.0.1:8443"}) {
			serveStatus = `{"TCP":{"443":{"TCPForward":"127.0.0.1:8443"}}}`
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected tailscale command %v", args)
	}}
	config := DefaultConfig()
	config.Tailnet = Tailnet{Domain: "dev.example.com", ZoneID: "zone", CredentialRef: "token", Email: "owner@example.com"}
	config.Projects["project"] = Project{Tailnet: true}
	p, err := s.Plan(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err = s.StoreCredential("token", []byte("fixture-token")); err != nil {
		t.Fatal(err)
	}
	authorized = false
	deleted = false
	setup, err := s.PlanSetup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	setup.TermsAccepted = true
	result, err := s.ApplySetup(ctx, setup)
	if err != nil {
		t.Fatal(err)
	}
	if wildcard == nil || !deleted || len(result.Steps) != 3 || result.Status.Certificate != "valid" {
		t.Fatalf("setup result %+v DNS=%+v challenge deleted=%v", result, wildcard, deleted)
	}
	cert, err = s.managedCertificate("dev.example.com", roots, now)
	if err != nil {
		t.Fatal(err)
	}
	before := cfReads
	renewed, err := s.RenewIfNeeded(ctx)
	if err != nil || renewed || cfReads != before {
		t.Fatalf("early renewal %v %v reads %d", renewed, err, cfReads-before)
	}
	clockNow = now.Add(31 * 24 * time.Hour)
	failChallenge = true
	authorized = false
	deleted = false
	if _, err = s.RenewIfNeeded(ctx); err == nil || !deleted {
		t.Fatalf("expected transient failure and owned TXT cleanup, got %v cleanup %v", err, deleted)
	}
	owned, err := s.readOwnership()
	if err != nil || owned.Pending != "" {
		t.Fatalf("safe failure remained blocked: %+v %v", owned, err)
	}
	failChallenge = false
	deleted = false
	renewed, err = s.RenewIfNeeded(ctx)
	if err != nil || !renewed {
		t.Fatalf("due renewal %v %v", renewed, err)
	}
	replacement, err := s.managedCertificate("dev.example.com", roots, clockNow)
	if err != nil || !replacement.Leaf.NotAfter.After(cert.Leaf.NotAfter) {
		t.Fatalf("renewed certificate: %v", err)
	}
}
