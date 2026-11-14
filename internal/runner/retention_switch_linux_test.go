//go:build linux

package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestRetentionAfterSuccessfulSwitches(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rdr")
	if b, e := exec.Command("go", "build", "-o", binary, "../../cmd/rdr").CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	main := filepath.Join(dir, "main")
	other := filepath.Join(dir, "other")
	if e := os.Mkdir(main, 0700); e != nil {
		t.Fatal(e)
	}
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = main
		if b, e := c.CombinedOutput(); e != nil {
			t.Fatalf("git: %v %s", e, b)
		}
	}
	git("init", "-q")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	cfg := "version = 1\n[project]\nname = 'demo'\n[services.worker]\ncommand = 'sleep 60'\n"
	if e := os.WriteFile(filepath.Join(main, "dev.toml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	git("add", "dev.toml")
	git("commit", "-qm", "initial")
	git("worktree", "add", "-qb", "other", other)
	p, e := discovery.Discover(main)
	if e != nil {
		t.Fatal(e)
	}
	var a, b *discovery.Worktree
	for i := range p.Worktrees {
		w := &p.Worktrees[i]
		if w.Root == main {
			a = w
		}
		if w.Root == other {
			b = w
		}
	}
	if a == nil || b == nil {
		t.Fatal("missing worktrees")
	}
	store, e := state.New(filepath.Join(dir, "state"))
	if e != nil {
		t.Fatal(e)
	}
	instances := map[string]state.Instance{}
	var trees []state.Worktree
	for _, w := range []*discovery.Worktree{a, b} {
		id := p.ID + "/" + w.ID + "/worker"
		instances[id] = state.Instance{ID: id, WorktreeID: w.ID, Service: "worker", State: "stopped"}
		trees = append(trees, state.Worktree{ID: w.ID, Root: w.Root})
	}
	if e = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: trees, Instances: instances})
	}); e != nil {
		t.Fatal(e)
	}
	r := New(store, binary)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	defer func() {
		for _, w := range []*discovery.Worktree{a, b} {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = r.Stop(cleanup, p, w, "worker")
			cancel()
		}
	}()
	if _, e = r.Up(ctx, p, a, "worker"); e != nil {
		t.Fatal(e)
	}
	from, to := a, b
	for n := 0; n < 16; n++ {
		result, err := r.SwitchGraph(ctx, []SwitchMapping{{From: Selection{p, from, "worker"}, To: Selection{p, to, "worker"}}})
		if err != nil {
			t.Fatalf("switch %d: %+v %v", n, result, err)
		}
		from, to = to, from
	}
	entries, e := os.ReadDir(filepath.Join(store.Root, "runs"))
	if e != nil {
		t.Fatal(e)
	}
	counts := map[string]int{}
	for _, entry := range entries {
		m, err := store.ReadManifest(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		counts[m.InstanceID]++
	}
	for _, w := range []*discovery.Worktree{a, b} {
		id := p.ID + "/" + w.ID + "/worker"
		if counts[id] != 6 {
			t.Fatalf("%s: expected current plus five runs, got %d", id, counts[id])
		}
	}
}
