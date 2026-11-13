//go:build linux

package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestRestartGraphPendingBindAndBuildFailure(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rdr")
	c := exec.Command("go", "build", "-o", binary, "../../cmd/rdr")
	if out, e := c.CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, out)
	}
	root := filepath.Join(dir, "main")
	other := filepath.Join(dir, "other")
	third := filepath.Join(dir, "third")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = root
		if out, e := c.CombinedOutput(); e != nil {
			t.Fatalf("git: %v %s", e, out)
		}
	}
	git("init", "-q")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	cfg := `version = 1
[project]
name = 'demo'
[services.api]
command = 'python3 -m http.server {port} --bind {host}'
port = 'auto'
[services.worker]
command = 'printf %s "$RDR_DEP_API_PORT" > started; sleep 60'
build = 'printf %s "$RDR_DEP_API_PORT" > built'
depends_on = ['api']
[services.broken]
command = 'sleep 60'
build = 'false'
depends_on = ['api']
`
	if e := os.WriteFile(filepath.Join(root, "dev.toml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	git("add", "dev.toml")
	git("commit", "-qm", "initial")
	git("worktree", "add", "-qb", "other", other)
	git("worktree", "add", "-qb", "third", third)
	p, e := discovery.Discover(root)
	if e != nil {
		t.Fatal(e)
	}
	var a, b, d *discovery.Worktree
	for i := range p.Worktrees {
		w := &p.Worktrees[i]
		if w.Root == root {
			a = w
		}
		if w.Root == other {
			b = w
		}
		if w.Root == third {
			d = w
		}
	}
	if a == nil || b == nil || d == nil {
		t.Fatal("missing worktree")
	}
	store, e := state.New(filepath.Join(dir, "state"))
	if e != nil {
		t.Fatal(e)
	}
	instances := map[string]state.Instance{}
	var trees []state.Worktree
	for _, w := range []*discovery.Worktree{a, b, d} {
		trees = append(trees, state.Worktree{ID: w.ID, Root: w.Root})
		for _, s := range []string{"api", "worker", "broken"} {
			id := p.ID + "/" + w.ID + "/" + s
			instances[id] = state.Instance{ID: id, Service: s, WorktreeID: w.ID, State: "stopped"}
		}
	}
	if e = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: trees, Instances: instances})
	}); e != nil {
		t.Fatal(e)
	}
	r := New(store, binary)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, e = r.Bind(ctx, Selection{p, b, "worker"}, "api", Selection{p, a, "api"}); e != nil {
		t.Fatal(e)
	}
	defer func() {
		for _, s := range []Selection{{p, b, "worker"}, {p, a, "worker"}, {p, d, "worker"}, {p, d, "api"}, {p, a, "api"}} {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = r.Stop(c, s.Project, s.Worktree, s.Service)
			cancel()
		}
	}()
	roots := []Selection{{p, a, "worker"}, {p, b, "worker"}, {p, d, "worker"}}
	first, e := r.UpGraph(ctx, roots)
	if e != nil {
		t.Fatalf("up: %+v %v", first, e)
	}
	old := map[string]string{}
	for _, v := range first.Results {
		old[v.InstanceID] = v.RunID
	}
	if _, e = r.Bind(ctx, Selection{p, b, "worker"}, "api", Selection{p, d, "api"}); e != nil {
		t.Fatal(e)
	}
	restarted, e := r.RestartGraph(ctx, roots)
	if e != nil {
		t.Fatalf("restart: %+v %v", restarted, e)
	}
	now := map[string]string{}
	for _, v := range restarted.Results {
		now[v.InstanceID] = v.RunID
	}
	for _, w := range []*discovery.Worktree{a, b, d} {
		id := p.ID + "/" + w.ID + "/worker"
		if now[id] == "" || now[id] == old[id] {
			t.Fatalf("not replaced %s: %+v", id, restarted)
		}
	}
	for _, w := range []*discovery.Worktree{a, d} {
		id := p.ID + "/" + w.ID + "/api"
		if now[id] != old[id] {
			t.Fatalf("dependency restarted %s", id)
		}
	}
	if _, e = r.RestartGraph(ctx, []Selection{{p, a, "worker"}, {p, b, "missing"}}); e == nil {
		t.Fatal("unknown selector accepted")
	}
	registry, e := store.Read()
	if e != nil {
		t.Fatal(e)
	}
	before, e := registry.Project(p.ID)
	if e != nil {
		t.Fatal(e)
	}
	if before.Instances[p.ID+"/"+a.ID+"/worker"].RunID != now[p.ID+"/"+a.ID+"/worker"] {
		t.Fatal("unknown selector changed run")
	}
	target := p.ID + "/" + d.ID + "/api"
	manifest, e := store.ReadManifest(now[p.ID+"/"+b.ID+"/worker"])
	if e != nil || manifest.EffectiveBindings["api"] != target {
		t.Fatalf("binding: %+v %v", manifest, e)
	}
	targetManifest, e := store.ReadManifest(now[target])
	if e != nil {
		t.Fatal(e)
	}
	port := targetManifest.Addresses["port"]
	for _, name := range []string{"built", "started"} {
		var data []byte
		for n := 0; n < 40; n++ {
			data, e = os.ReadFile(filepath.Join(b.Root, name))
			if e == nil && string(data) == port {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		if e != nil || string(data) != port {
			t.Fatalf("%s: %q want %q: %v", name, data, port, e)
		}
	}
	apiRestart, e := r.RestartGraph(ctx, []Selection{{p, d, "api"}})
	if e != nil {
		t.Fatalf("restart dependency: %+v %v", apiRestart, e)
	}
	if len(apiRestart.Results) != 1 || apiRestart.Results[0].URLs["port"] != port || apiRestart.Results[0].RunID == now[target] {
		t.Fatalf("auto port not reused: %+v", apiRestart)
	}
	now[target] = apiRestart.Results[0].RunID
	failedConfig := strings.Replace(cfg, "build = 'printf %s \"$RDR_DEP_API_PORT\" > built'", "build = 'false'", 1)
	if failedConfig == cfg {
		t.Fatal("fixture replacement failed")
	}
	if e = os.WriteFile(filepath.Join(b.Root, "dev.toml"), []byte(failedConfig), 0600); e != nil {
		t.Fatal(e)
	}
	failed, e := r.RestartGraph(ctx, roots)
	if e == nil {
		t.Fatalf("expected build error: %+v", failed)
	}
	reg, e := store.Read()
	if e != nil {
		t.Fatal(e)
	}
	project, e := reg.Project(p.ID)
	if e != nil {
		t.Fatal(e)
	}
	for _, w := range []*discovery.Worktree{a, b, d} {
		id := p.ID + "/" + w.ID + "/worker"
		if project.Instances[id].RunID != now[id] {
			t.Fatalf("old run lost after failed build: %s", id)
		}
	}
}
