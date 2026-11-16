//go:build linux

// Tests runner graph down behavior on Linux.
package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestDownGraphCascade(t *testing.T) {
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
	result, e := r.UpGraph(ctx, []Selection{{p, a, "worker"}, {p, b, "worker"}, {p, d, "worker"}})
	if e != nil {
		t.Fatalf("up: %+v %v", result, e)
	}
	if _, e = r.DownGraph(ctx, []Selection{{p, a, "api"}}, false); e == nil {
		t.Fatal("guard accepted live consumers")
	}
	if _, e = r.DownGraph(ctx, []Selection{{p, a, "worker"}, {p, d, "missing"}}, false); e == nil {
		t.Fatal("unknown selector accepted")
	}
	check, e := r.PlanGraph(ctx, "up", []Selection{{p, a, "worker"}}, false)
	if e != nil || check.Instances[len(check.Instances)-1].State != "running" {
		t.Fatalf("invalid request changed state: %+v %v", check, e)
	}
	if e = os.Remove(filepath.Join(root, "dev.toml")); e != nil {
		t.Fatal(e)
	}
	down, e := r.DownGraph(ctx, []Selection{{p, a, "api"}, {p, d, "worker"}}, true)
	if e != nil {
		t.Fatalf("down: %+v %v", down, e)
	}
	if len(down.Results) != 4 {
		t.Fatalf("cascade: %+v", down)
	}
	for _, v := range down.Results {
		if v.State != "stopped" {
			t.Fatalf("not stopped: %+v", v)
		}
	}
}

func TestDownGraphFailureBlocksDependency(t *testing.T) {
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
	result, e := r.UpGraph(ctx, []Selection{{p, a, "worker"}, {p, d, "worker"}})
	if e != nil {
		t.Fatal(e)
	}
	workerID := p.ID + "/" + a.ID + "/worker"
	apiID := p.ID + "/" + a.ID + "/api"
	independentID := p.ID + "/" + d.ID + "/worker"
	var runID string
	for _, v := range result.Results {
		if v.InstanceID == workerID {
			runID = v.RunID
		}
	}
	if runID == "" {
		t.Fatal("missing run")
	}
	path := filepath.Join(store.Root, "runs", runID, "manifest.json")
	original, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var m state.Manifest
	if e = json.Unmarshal(original, &m); e != nil {
		t.Fatal(e)
	}
	m.Launch.StopTimeout = "invalid"
	corrupt, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, corrupt, 0600); e != nil {
		t.Fatal(e)
	}
	defer os.WriteFile(path, original, 0600)
	down, e := r.DownGraph(ctx, []Selection{{p, a, "api"}, {p, d, "worker"}}, true)
	if e == nil {
		t.Fatal("expected stop error")
	}
	seen := map[string]Result{}
	for _, v := range down.Results {
		seen[v.InstanceID] = v
	}
	if seen[workerID].Status != "failed" || seen[apiID].Status != "not_run" || seen[independentID].Status != "completed" {
		t.Fatalf("failure propagation: %+v", down)
	}
}

func TestGraphStateReportsMissingManifest(t *testing.T) {
	store, e := state.New(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	id := "project/worktree/service"
	e = store.Update(func(reg *state.Registry) error {
		return reg.SetProject("project", state.Project{ID: "project", Name: "demo", Root: "/tmp", Worktrees: []state.Worktree{{ID: "worktree", Root: "/tmp"}}, Instances: map[string]state.Instance{id: {ID: id, Service: "service", WorktreeID: "worktree", State: "running", RunID: "missing"}}})
	})
	if e != nil {
		t.Fatal(e)
	}
	observed, run, e := New(store, "").graphState(id)
	if e == nil || observed != "" || run != "missing" {
		t.Fatalf("state: %q run: %q error: %v", observed, run, e)
	}
}

func TestDownGraphRecoversPreparationWithoutManifest(t *testing.T) {
	store, e := state.New(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	p := &discovery.Project{ID: "project"}
	w := &discovery.Worktree{ID: "worktree", Root: "/tmp"}
	id := "project/worktree/service"
	e = store.Update(func(reg *state.Registry) error {
		return reg.SetProject("project", state.Project{ID: "project", Name: "demo", Root: "/tmp", Worktrees: []state.Worktree{{ID: "worktree", Root: "/tmp"}}, Instances: map[string]state.Instance{id: {ID: id, Service: "service", WorktreeID: "worktree", State: "stopped", Preparation: &state.Intent{RunID: "pending", OperationID: "op", Phase: "preparing"}}}})
	})
	if e != nil {
		t.Fatal(e)
	}
	result, e := New(store, "").DownGraph(context.Background(), []Selection{{p, w, "service"}}, false)
	if e != nil || len(result.Results) != 1 {
		t.Fatalf("down: %+v %v", result, e)
	}
	reg, e := store.Read()
	if e != nil {
		t.Fatal(e)
	}
	project, e := reg.Project("project")
	if e != nil || project.Instances[id].Preparation != nil {
		t.Fatalf("preparation retained: %v", e)
	}
}
