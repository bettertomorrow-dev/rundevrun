//go:build linux

// Tests runner switch failure behavior on Linux.
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

func TestSwitchGraphBuildFailureKeepsOldRuns(t *testing.T) {
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
	first, e := r.UpGraph(ctx, []Selection{{p, a, "worker"}, {p, b, "worker"}, {p, d, "worker"}})
	if e != nil {
		t.Fatalf("up: %+v %v", first, e)
	}
	ids := map[string]string{}
	for _, v := range first.Results {
		ids[v.InstanceID] = v.RunID
	}
	failed := strings.Replace(cfg, "build = 'printf %s \"$RDR_DEP_API_PORT\" > built'", "build = 'false'", 1)
	if failed == cfg {
		t.Fatal("fixture replacement failed")
	}
	if e = os.WriteFile(filepath.Join(b.Root, "dev.toml"), []byte(failed), 0600); e != nil {
		t.Fatal(e)
	}
	result, e := r.SwitchGraph(ctx, []SwitchMapping{{From: Selection{p, a, "api"}, To: Selection{p, b, "api"}}})
	if e == nil {
		t.Fatalf("expected failed build: %+v", result)
	}
	failedConsumer := p.ID + "/" + b.ID + "/worker"
	found := false
	for _, item := range result.Results {
		if item.InstanceID == failedConsumer {
			found = true
			if item.Status != "failed" || item.State != "running" {
				t.Fatalf("failed preparation lost actual consumer state: %+v", item)
			}
		}
	}
	if !found {
		t.Fatalf("failed consumer missing from results: %+v", result)
	}
	journal, err := store.ReadOperation(result.OperationID)
	if err != nil || journal.Status != "failed" {
		t.Fatalf("journal: %+v %v", journal, err)
	}
	reg, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	project, err := reg.Project(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	for id, run := range ids {
		if project.Instances[id].RunID != run {
			t.Fatalf("old run changed: %s", id)
		}
	}
}
