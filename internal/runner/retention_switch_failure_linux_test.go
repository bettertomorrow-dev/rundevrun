//go:build linux

package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestSwitchPruneFailureKeepsActualResult(t *testing.T) {
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
	bad := filepath.Join(store.Root, "operations", "bad.json")
	targetConfig := cfg + "env = { TARGET = " + strconv.Quote(bad) + " }\nhooks = { after_start = 'touch \"$TARGET\"' }\n"
	if e = os.WriteFile(filepath.Join(b.Root, "dev.toml"), []byte(targetConfig), 0600); e != nil {
		t.Fatal(e)
	}
	result, err := r.SwitchGraph(ctx, []SwitchMapping{{From: Selection{p, a, "worker"}, To: Selection{p, b, "worker"}}})
	defer os.Remove(bad)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "IO_ERROR" {
		t.Fatalf("expected prune IO error: %+v %v", result, err)
	}
	targetID := p.ID + "/" + b.ID + "/worker"
	found := false
	for _, entry := range result.Results {
		if entry.InstanceID == targetID {
			found = true
			if entry.State != "running" || entry.Status != "completed" {
				t.Fatalf("actual target result: %+v", entry)
			}
		}
	}
	if !found {
		t.Fatal("missing actual result")
	}
	journal, e := store.ReadOperation(result.OperationID)
	if e != nil || journal.Status != "completed" {
		t.Fatalf("journal should stay completed: %+v %v", journal, e)
	}
}
