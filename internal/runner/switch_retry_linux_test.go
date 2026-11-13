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

func TestSwitchRetryHookUsesTargetWorktree(t *testing.T) {
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
	cfg = strings.Replace(cfg, "[services.api]\n", "[services.api]\nhooks = { before_stop = 'if [ -f flag ]; then echo $RDR_NEW_WORKTREE > retry-path; else touch flag; exit 1; fi' }\n", 1)
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

	defer func() {
		for _, s := range []Selection{{p, b, "worker"}, {p, a, "worker"}, {p, d, "worker"}, {p, d, "api"}, {p, a, "api"}} {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = r.Stop(c, s.Project, s.Worktree, s.Service)
			cancel()
		}
	}()
	defer func() {
		reg, err := store.Read()
		if err != nil {
			return
		}
		project, err := reg.Project(p.ID)
		if err != nil {
			return
		}
		for _, w := range []*discovery.Worktree{a, b} {
			id := p.ID + "/" + w.ID + "/api"
			inst := project.Instances[id]
			if inst.RunID != "" {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = r.stopLocked(cleanup, p.ID, id, inst.RunID)
				cancel()
			}
		}
	}()
	if _, e = r.Up(ctx, p, a, "api"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Stop(ctx, p, a, "api"); e == nil {
		t.Fatal("expected hook failure")
	}
	r.RetryHooks = true
	result, e := r.SwitchGraph(ctx, []SwitchMapping{{From: Selection{p, a, "api"}, To: Selection{p, b, "api"}}})
	if e != nil {
		t.Fatalf("switch retry: %+v %v", result, e)
	}
	data, e := os.ReadFile(filepath.Join(a.Root, "retry-path"))
	if e != nil || strings.TrimSpace(string(data)) != b.Root {
		t.Fatalf("retry new worktree: %q want %q %v", data, b.Root, e)
	}
	found := false
	for _, step := range result.Steps {
		if strings.HasPrefix(step.ID, "recover_hook:") && step.Status == "completed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("retry boundary missing: %+v", result.Steps)
	}
}
