//go:build linux

// Tests runner routing behavior on Linux.
package runner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func routingFixture(t *testing.T) (*Runner, *discovery.Project, *discovery.Worktree, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "main")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	config := `version = 1
[project]
name = 'demo'
[routing]
domain = 'dev.example.com'
[services.api]
command = 'python3 -m http.server {port} --bind {host}'
build = 'printenv RDR_URL RDR_DIRECT_URL > api.build'
port = 'auto'
[services.worker]
command = 'printenv API_ROUTE API_DIRECT > start.out; sleep 60'
build = 'printenv API_ROUTE API_DIRECT > build.out'
depends_on = ['api']
env = { API_ROUTE = '{dependencies.api.url}', API_DIRECT = '{dependencies.api.direct_url}' }
`
	if err := os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := discovery.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	w := &p.Worktrees[0]
	store, err := state.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	instances := map[string]state.Instance{}
	for _, svc := range []string{"api", "worker"} {
		id := p.ID + "/" + w.ID + "/" + svc
		instances[id] = state.Instance{ID: id, Service: svc, WorktreeID: w.ID, State: "stopped"}
	}
	if err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: instances})
	}); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	crt := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"*.dev.example.com"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, crt, crt, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "rundevrun")
	cmd := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun")
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, out)
	}
	r := New(store, binary)
	r.Routing = &routing.Config{Routing: routing.Settings{Enabled: true, Domain: "else.example.com", Hostname: "{service}-{worktree}-{project}.{domain}", HTTPSListen: "127.0.0.1:8443", TLS: routing.TLSSettings{Certificates: []routing.Certificate{{CertFile: certPath, KeyFile: keyPath}}}}}
	return r, p, w, root
}
func TestRoutingGraphActualURLs(t *testing.T) {
	r, p, w, root := routingFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	selection := Selection{p, w, "worker"}
	before, err := os.ReadFile(filepath.Join(r.Store.Root, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.PlanGraph(ctx, "up", []Selection{selection}, false)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(r.Store.Root, "registry.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("dry run wrote registry")
	}
	if len(plan.Instances) != 2 || plan.Instances[0].Addresses["url"] != "https://api-main-demo.dev.example.com:8443" || !strings.HasPrefix(plan.Instances[0].Addresses["direct_url"], "http://127.0.0.1:") {
		t.Fatalf("plan: %+v", plan)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_, _ = r.Stop(c, p, w, "worker")
		_, _ = r.Stop(c, p, w, "api")
	}()
	result, err := r.UpGraph(ctx, []Selection{selection})
	if err != nil {
		t.Fatalf("up: %+v %v", result, err)
	}
	var api Result
	for _, v := range result.Results {
		if strings.HasSuffix(v.InstanceID, "/api") {
			api = v
		}
	}
	if api.URLs["url"] != "https://api-main-demo.dev.example.com:8443" || !strings.HasPrefix(api.URLs["direct_url"], "http://127.0.0.1:") {
		t.Fatalf("api URLs: %+v", api.URLs)
	}
	expected := api.URLs["url"] + "\n" + api.URLs["direct_url"] + "\n"
	for _, name := range []string{"api.build", "build.out", "start.out"} {
		data, e := os.ReadFile(filepath.Join(root, name))
		if e != nil || string(data) != expected {
			t.Fatalf("%s = %q, expected %q: %v", name, data, expected, e)
		}
	}
	reg, err := r.Store.Read()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reg.Project(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	apiInst := saved.Instances[api.InstanceID]
	if apiInst.RouteURL != api.URLs["url"] || saved.Worktrees[0].Alias != "main" {
		t.Fatalf("route book: %+v %+v", apiInst, saved.Worktrees)
	}
	r.Routing.Routing.HTTPSListen = "127.0.0.1:9443"
	again, err := r.UpGraph(ctx, []Selection{selection})
	if err != nil {
		t.Fatal(err)
	}
	if again.Results[0].RunID != api.RunID || again.Results[0].URLs["url"] != "https://api-main-demo.dev.example.com:9443" {
		t.Fatalf("live route drift: %+v", again)
	}
	old, err := os.ReadFile(filepath.Join(root, "api.build"))
	if err != nil || string(old) != expected {
		t.Fatal("unchanged process environment changed")
	}
	changed := "version = 1\n[project]\nname = 'demo'\n[services.api]\ncommand = 'sleep 60'\n"
	if err = os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	fresh, err := discovery.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	current, err := discovery.Resolve(fresh, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := r.PlanGraph(ctx, "up", []Selection{{fresh, current, "api"}}, false)
	if err != nil || unchanged.Instances[0].RunID != api.RunID || unchanged.Instances[0].RouteURL != "https://api-main-demo.dev.example.com:9443" {
		t.Fatalf("current process config replaced a live HTTP route: %+v, %v", unchanged, err)
	}
	reg, err = r.Store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err = r.AssignProjectRoutes(&reg, fresh); err != nil {
		t.Fatal(err)
	}
	registered, err := reg.Project(p.ID)
	if err != nil || registered.Instances[api.InstanceID].RouteURL != "https://api-main-demo.dev.example.com:9443" {
		t.Fatalf("registration replaced a live HTTP route: %+v, %v", registered.Instances[api.InstanceID], err)
	}
}
func TestRouteValidationBeforeWrites(t *testing.T) {
	r, p, w, root := routingFixture(t)
	ctx := context.Background()
	api := p.ID + "/" + w.ID + "/api"
	before, err := os.ReadFile(filepath.Join(r.Store.Root, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	r.Routing.Routing.TLS.Certificates[0].KeyFile = "/missing/private-key"
	_, err = r.PlanGraph(ctx, "up", []Selection{{p, w, "worker"}}, false)
	var re *Error
	if !errors.As(err, &re) || re.Code != "TLS_INVALID" {
		t.Fatalf("TLS validation: %v", err)
	}
	if _, err = r.UpGraph(ctx, []Selection{{p, w, "worker"}}); !errors.As(err, &re) || re.Code != "TLS_INVALID" {
		t.Fatalf("invalid TLS up: %v", err)
	}
	if _, err = os.Stat(filepath.Join(root, "api.build")); !os.IsNotExist(err) {
		t.Fatalf("build ran on TLS failure: %v", err)
	}
	if _, err = os.Stat(filepath.Join(r.Store.Root, "runs")); !os.IsNotExist(err) {
		t.Fatalf("created runs: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(r.Store.Root, "registry.json"))
	if string(before) != string(after) {
		t.Fatal("invalid TLS wrote registry")
	}
	r.Routing.Routing.Enabled = false
	disabled, err := r.PlanGraph(ctx, "up", []Selection{{p, w, "worker"}}, false)
	if err != nil || disabled.Instances[0].Addresses["url"] != disabled.Instances[0].Addresses["direct_url"] {
		t.Fatalf("disabled routing used keys: %+v %v", disabled, err)
	}
	r.Routing.Routing.Enabled = true
	r.Routing.Routing.TLS.Certificates[0].KeyFile = filepath.Join(filepath.Dir(r.Routing.Routing.TLS.Certificates[0].CertFile), "key.pem")
	if err = r.Store.Update(func(reg *state.Registry) error {
		project, e := reg.Project(p.ID)
		if e != nil {
			return e
		}
		v := project.Instances[api]
		v.Hostname = "api-main-demo.dev.example.com"
		project.Instances[api] = v
		id := p.ID + "/" + w.ID + "/other"
		project.Instances[id] = state.Instance{ID: id, Service: "other", WorktreeID: w.ID, State: "stopped", Hostname: v.Hostname}
		return reg.SetProject(p.ID, project)
	}); err != nil {
		t.Fatal(err)
	}
	_, err = r.PlanGraph(ctx, "up", []Selection{{p, w, "api"}}, false)
	if !errors.As(err, &re) || re.Code != "ROUTE_CONFLICT" {
		t.Fatalf("collision accepted: %v", err)
	}
}

func TestAssignProjectRoutesAtomicAndStable(t *testing.T) {
	r, p, w, _ := routingFixture(t)
	reg, err := r.Store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err = r.AssignProjectRoutes(&reg, p); err != nil {
		t.Fatal(err)
	}
	saved, err := reg.Project(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	id := p.ID + "/" + w.ID + "/api"
	original := saved.Instances[id]
	if original.Hostname != "api-main-demo.dev.example.com" || saved.Worktrees[0].Alias != "main" {
		t.Fatalf("registration: %+v %+v", original, saved.Worktrees)
	}
	r.Routing.Routing.Hostname = "{service}-changed.{domain}"
	if err = r.AssignProjectRoutes(&reg, p); err != nil {
		t.Fatal(err)
	}
	saved, err = reg.Project(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Instances[id].Hostname != original.Hostname {
		t.Fatal("existing hostname changed with template")
	}
	r.Routing.Routing.Enabled = false
	if err = r.AssignProjectRoutes(&reg, p); err != nil {
		t.Fatal(err)
	}
	saved, err = reg.Project(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Instances[id].Hostname != original.Hostname || saved.Instances[id].RouteURL != "" {
		t.Fatal("disabled routing lost name or retained URL")
	}
	r.Routing.Routing.Enabled = true
	r.Routing.Routing.TLS.Certificates[0].KeyFile = "/missing"
	before, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.AssignProjectRoutes(&reg, p); err == nil {
		t.Fatal("accepted invalid certificate")
	}
	after, err := json.Marshal(reg)
	if err != nil || string(before) != string(after) {
		t.Fatal("failed registration mutated registry")
	}
}

func TestAllReservedHostsRequireCertificate(t *testing.T) {
	r, p, w, _ := routingFixture(t)
	err := r.Store.Update(func(reg *state.Registry) error {
		project, e := reg.Project(p.ID)
		if e != nil {
			return e
		}
		id := p.ID + "/" + w.ID + "/worker"
		v := project.Instances[id]
		v.Hostname = "old.other.example.com"
		project.Instances[id] = v
		return reg.SetProject(p.ID, project)
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.PlanGraph(context.Background(), "up", []Selection{{p, w, "api"}}, false)
	var re *Error
	if !errors.As(err, &re) || re.Code != "TLS_INVALID" {
		t.Fatalf("uncovered existing route accepted: %v", err)
	}
}
func TestKnownLiveRouteDoesNotReadWorktreeConfig(t *testing.T) {
	r, p, w, _ := routingFixture(t)
	reg, err := r.Store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if err = r.AssignProjectRoutes(&reg, p); err != nil {
		t.Fatal(err)
	}
	id := p.ID + "/" + w.ID + "/api"
	saved, err := reg.Project(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	expected := saved.Instances[id]
	s := Selection{Project: &discovery.Project{ID: p.ID}, Worktree: &discovery.Worktree{ID: w.ID, Root: w.Root}, Service: "api"}
	routes, err := r.assignRoutes(&reg, map[string]Selection{id: s})
	if err != nil {
		t.Fatal(err)
	}
	if routes[id].host != expected.Hostname || routes[id].url != expected.RouteURL {
		t.Fatalf("changed saved route: %+v", routes[id])
	}
}

func TestSharedRouteSurvivesInvalidLiveWorktreeConfig(t *testing.T) {
	r, _, _, root := routingFixture(t)
	other := filepath.Join(filepath.Dir(root), "feature")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	git("add", "rundevrun.toml")
	git("commit", "-qm", "fixture")
	git("worktree", "add", "-qb", "feature", other)
	p, err := discovery.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	var mainW, otherW *discovery.Worktree
	for i := range p.Worktrees {
		w := &p.Worktrees[i]
		if w.Root == root {
			mainW = w
		}
		if w.Root == other {
			otherW = w
		}
	}
	if mainW == nil || otherW == nil {
		t.Fatal("missing worktrees")
	}
	store, err := state.New(filepath.Join(filepath.Dir(root), "git-state"))
	if err != nil {
		t.Fatal(err)
	}
	r.Store = store
	instances := map[string]state.Instance{}
	var trees []state.Worktree
	for _, w := range []*discovery.Worktree{mainW, otherW} {
		trees = append(trees, state.Worktree{ID: w.ID, Root: w.Root})
		for _, svc := range []string{"api", "worker"} {
			id := p.ID + "/" + w.ID + "/" + svc
			instances[id] = state.Instance{ID: id, Service: svc, WorktreeID: w.ID, State: "stopped"}
		}
	}
	if err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: trees, Instances: instances})
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	defer func() {
		c, stop := context.WithTimeout(context.Background(), 8*time.Second)
		defer stop()
		_, _ = r.Stop(c, p, otherW, "worker")
		_, _ = r.Stop(c, p, mainW, "api")
	}()
	if _, err = r.UpGraph(ctx, []Selection{{p, mainW, "api"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Bind(ctx, Selection{p, otherW, "worker"}, "api", Selection{p, mainW, "api"}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte("invalid = ["), 0600); err != nil {
		t.Fatal(err)
	}
	fresh, err := discovery.Discover(other)
	if err != nil {
		t.Fatal(err)
	}
	fw, err := discovery.Resolve(fresh, otherW.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.PlanGraph(ctx, "up", []Selection{{fresh, fw, "worker"}}, false)
	if err != nil {
		t.Fatalf("invalid live dependency config prevented plan: %v", err)
	}
	if len(plan.Instances) != 2 || plan.Instances[0].RouteURL != "https://api-main-demo.dev.example.com:8443" || plan.Instances[1].env["RDR_DEP_API_URL"] != plan.Instances[0].RouteURL {
		t.Fatalf("shared route: %+v", plan)
	}
	result, err := r.UpGraph(ctx, []Selection{{fresh, fw, "worker"}})
	if err != nil {
		t.Fatalf("shared dependency reuse: %+v %v", result, err)
	}
	independent, err := r.PlanGraph(ctx, "up", []Selection{{fresh, fw, "api"}}, false)
	if err != nil || independent.Instances[0].RouteURL != "https://api-feature-demo.dev.example.com:8443" {
		t.Fatalf("independent route: %+v %v", independent, err)
	}
}

func TestNormalizedHostnameCollisionHasNoTakeover(t *testing.T) {
	r, p, w, _ := routingFixture(t)
	reg, err := r.Store.Read()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reg.Project(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	other := discovery.Worktree{ID: "other", Root: "/another/main", Config: w.Config}
	id := p.ID + "/other/api"
	saved.Worktrees = append(saved.Worktrees, state.Worktree{ID: other.ID, Root: other.Root})
	saved.Instances[id] = state.Instance{ID: id, Service: "api", WorktreeID: other.ID, State: "stopped"}
	if err = reg.SetProject(p.ID, saved); err != nil {
		t.Fatal(err)
	}
	fresh := *p
	fresh.Worktrees = append(append([]discovery.Worktree(nil), p.Worktrees...), other)
	before, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	err = r.AssignProjectRoutes(&reg, &fresh)
	var re *Error
	if !errors.As(err, &re) || re.Code != "ROUTE_CONFLICT" {
		t.Fatalf("collision: %v", err)
	}
	after, err := json.Marshal(reg)
	if err != nil || string(before) != string(after) {
		t.Fatal("collision mutated registry")
	}
}
