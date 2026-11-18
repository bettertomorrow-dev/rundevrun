// Tests runner alias behavior.
package runner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func aliasFixture(t *testing.T) (*Runner, *discovery.Project, *discovery.Worktree) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "main")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}

	content := `version=1
[project]
name='demo'
[routing]
domain='dev.example.com'
[services.api]
command='sleep 60'
[services.web]
command='echo web'
port='auto'
[services.worker]
command='echo worker'
`
	if err := os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := state.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	p := &discovery.Project{ID: "p", Name: "demo", Root: root}
	w := &discovery.Worktree{ID: "w", Root: root}
	instances := map[string]state.Instance{}
	for _, svc := range []string{"api", "web", "worker"} {
		id := "p/w/" + svc
		instances[id] = state.Instance{ID: id, Service: svc, WorktreeID: "w", State: "stopped"}
	}
	api := instances["p/w/api"]
	api.State = "starting"
	api.RunID = "active"
	api.Hostname = "api-old-demo.dev.example.com"
	api.RouteURL = "https://api-old-demo.dev.example.com:8443"
	instances[api.ID] = api
	web := instances["p/w/web"]
	web.Hostname = "web-old-demo.dev.example.com"
	web.RouteURL = "https://web-old-demo.dev.example.com:8443"
	instances[web.ID] = web
	instances["p/other/web"] = state.Instance{ID: "p/other/web", Service: "web", WorktreeID: "other", State: "stopped", Hostname: "web-other-demo.dev.example.com", RouteURL: "https://web-other-demo.dev.example.com:8443"}
	if err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject("p", state.Project{ID: "p", Name: "demo", Root: root, Worktrees: []state.Worktree{{ID: "w", Root: root, Alias: "old"}, {ID: "other", Root: filepath.Join(dir, "other"), Alias: "other"}}, Instances: instances})
	}); err != nil {
		t.Fatal(err)
	}
	self, err := procid.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	manifest := state.Manifest{RunID: "active", InstanceID: api.ID, Launch: state.LaunchContract{Protocol: "http"}, Addresses: map[string]string{"url": api.RouteURL, "direct_url": "http://127.0.0.1:23456"}}
	if err = store.WriteManifest("active", manifest); err != nil {
		t.Fatal(err)
	}
	if err = store.WriteRun("active", state.RunRecord{Child: self, State: "running"}); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"*.dev.example.com"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cf, kf := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err = os.WriteFile(cf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(kf, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}), 0600); err != nil {
		t.Fatal(err)
	}
	r := New(store, "")
	r.Routing = &routing.Config{Routing: routing.Settings{Enabled: true, Domain: "ignored.example.com", Hostname: "{service}-{worktree}-{project}.{domain}", HTTPSListen: "127.0.0.1:8443", TLS: routing.TLSSettings{Certificates: []routing.Certificate{{CertFile: cf, KeyFile: kf}}}}}
	return r, p, w
}
func TestAliasPlanAndCommit(t *testing.T) {
	r, p, w := aliasFixture(t)
	path := filepath.Join(r.Store.Root, "registry.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.PlanAlias(context.Background(), p, w, "Feature")
	if err != nil {
		t.Fatal(err)
	}
	if plan.NewAlias != "feature" || plan.OldAlias != "old" || len(plan.Changes) != 2 {
		t.Fatalf("plan: %+v", plan)
	}
	if plan.Changes[0].NewHostname != "api-feature-demo.dev.example.com" || !plan.Changes[0].RestartRequired || plan.Changes[1].RestartRequired {
		t.Fatalf("changes: %+v", plan.Changes)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("alias plan wrote state")
	}
	committed, err := r.SetAlias(context.Background(), p, w, "Feature")
	if err != nil {
		t.Fatal(err)
	}
	if committed.Revision != plan.Revision {
		t.Fatalf("unexpected revision: %+v", committed)
	}
	reg, err := r.Store.Read()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reg.Project(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Worktrees[0].Alias != "feature" || saved.Instances["p/w/api"].Hostname != "api-feature-demo.dev.example.com" || saved.Instances["p/w/api"].RunID != "active" || saved.Instances["p/other/web"].Hostname != "web-other-demo.dev.example.com" {
		t.Fatalf("committed: %+v", saved)
	}
	manifest, err := r.Store.ReadManifest("active")
	if err != nil || manifest.Addresses["url"] != "https://api-old-demo.dev.example.com:8443" {
		t.Fatalf("active manifest changed: %+v %v", manifest, err)
	}

	r.Routing.Routing.Hostname = "{service}-new-{worktree}.{domain}"
	again, err := r.PlanAlias(context.Background(), p, w, "feature")
	if err != nil || again.Changes[0].NewHostname != "api-feature-demo.dev.example.com" {
		t.Fatalf("same alias renamed: %+v %v", again, err)
	}
}
func TestAliasRejectsInvalidCandidatesWithoutWrites(t *testing.T) {
	r, p, w := aliasFixture(t)
	path := filepath.Join(r.Store.Root, "registry.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"", "-bad", "bad_thing", strings.Repeat("x", 64), "other"} {
		if _, err = r.SetAlias(context.Background(), p, w, label); err == nil {
			t.Fatalf("accepted alias %q", label)
		}
		after, e := os.ReadFile(path)
		if e != nil || string(before) != string(after) {
			t.Fatalf("bad alias %q wrote registry", label)
		}
	}
	r.Routing.Routing.TLS.Certificates[0].KeyFile = "/nonexistent/key"
	_, err = r.SetAlias(context.Background(), p, w, "feature")
	var re *Error
	if !errors.As(err, &re) || re.Code != "TLS_INVALID" {
		t.Fatalf("invalid certificate: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("invalid TLS wrote registry")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = r.PlanAlias(ctx, p, w, "feature")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if err = os.WriteFile(filepath.Join(w.Root, "rundevrun.toml"), []byte("broken = ["), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = r.PlanAlias(context.Background(), p, w, "feature")
	if !errors.As(err, &re) || re.Code != "CONFIG_INVALID" {
		t.Fatalf("bad configuration: %v", err)
	}
}
