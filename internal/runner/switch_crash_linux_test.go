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

// A separate frontend process lets SIGKILL interrupt real journal boundaries.
func TestSwitchFrontendChild(t *testing.T) {
	idx := -1
	for i, arg := range os.Args {
		if arg == "--" {
			idx = i
			break
		}
	}
	if idx < 0 || len(os.Args) != idx+5 {
		t.Skip("frontend helper")
	}
	main, other, root, binary := os.Args[idx+1], os.Args[idx+2], os.Args[idx+3], os.Args[idx+4]
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
		t.Fatal("worktree missing")
	}
	_, e = New(&state.Store{Root: root}, binary).SwitchGraph(context.Background(), []SwitchMapping{{From: Selection{p, a, "api"}, To: Selection{p, b, "api"}}})
	if e != nil {
		t.Fatal(e)
	}
}
func TestSwitchJournalSurvivesKilledFrontend(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "rdr")
	if b, e := exec.Command("go", "build", "-o", binary, "../../cmd/rdr").CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	for _, stage := range []string{"prepare", "stop", "start"} {
		t.Run(stage, func(t *testing.T) {
			dir := t.TempDir()
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
			base := "version = 1\n[project]\nname = 'demo'\n[services.api]\ncommand = 'sleep 60'\n"
			if stage == "stop" {
				base += "hooks = { before_stop = 'echo invoked >> marker; sleep 60' }\n"
			}
			if e := os.WriteFile(filepath.Join(main, "dev.toml"), []byte(base), 0600); e != nil {
				t.Fatal(e)
			}
			git("add", "dev.toml")
			git("commit", "-qm", "initial")
			git("worktree", "add", "-qb", "other", other)
			target := "version = 1\n[project]\nname = 'demo'\n[services.api]\ncommand = 'sleep 60'\n"
			if stage == "prepare" {
				target += "build = 'echo invoked >> marker; sleep 60'\n"
			}
			if stage == "start" {
				target += "hooks = { before_start = 'echo invoked >> marker; sleep 60' }\n"
			}
			if e := os.WriteFile(filepath.Join(other, "dev.toml"), []byte(target), 0600); e != nil {
				t.Fatal(e)
			}
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
				t.Fatal("worktree missing")
			}
			store, e := state.New(filepath.Join(dir, "state"))
			if e != nil {
				t.Fatal(e)
			}
			instances := map[string]state.Instance{}
			var trees []state.Worktree
			for _, w := range []*discovery.Worktree{a, b} {
				id := p.ID + "/" + w.ID + "/api"
				instances[id] = state.Instance{ID: id, Service: "api", WorktreeID: w.ID, State: "stopped"}
				trees = append(trees, state.Worktree{ID: w.ID, Root: w.Root})
			}
			if e = store.Update(func(reg *state.Registry) error {
				return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: trees, Instances: instances})
			}); e != nil {
				t.Fatal(e)
			}
			r := New(store, binary)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, e = r.Up(ctx, p, a, "api"); e != nil {
				t.Fatal(e)
			}
			defer func() {
				reg, e := store.Read()
				if e != nil {
					return
				}
				project, e := reg.Project(p.ID)
				if e != nil {
					return
				}
				for _, w := range []*discovery.Worktree{a, b} {
					id := p.ID + "/" + w.ID + "/api"
					i := project.Instances[id]
					if i.Preparation != nil {
						_ = r.recoverPreparation(context.Background(), p.ID, id)
					}
					if i.RunID != "" {
						cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						_ = r.stopLocked(cleanup, p.ID, id, i.RunID)
						cancel()
					}
				}
			}()
			child := exec.Command(os.Args[0], "-test.run=^TestSwitchFrontendChild$", "--", main, other, store.Root, binary)
			if e = child.Start(); e != nil {
				t.Fatal(e)
			}
			defer func() {
				if child.Process != nil {
					_ = child.Process.Kill()
					_, _ = child.Process.Wait()
				}
			}()
			markerRoot := other
			if stage == "stop" {
				markerRoot = main
			}
			marker := filepath.Join(markerRoot, "marker")
			for n := 0; n < 300; n++ {
				if _, e = os.Stat(marker); e == nil {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			if e != nil {
				t.Fatal("boundary was not reached")
			}
			if e = child.Process.Kill(); e != nil {
				t.Fatal(e)
			}
			_ = child.Wait()
			child.Process = nil
			operations, e := store.ListOperations()
			if e != nil || len(operations) != 1 {
				t.Fatalf("operations: %+v %v", operations, e)
			}
			op := operations[0]
			if op.Status != "running" {
				t.Fatalf("journal status: %s", op.Status)
			}
			expected := stage + ":" + p.ID + "/" + b.ID + "/api"
			if stage == "stop" {
				expected = stage + ":" + p.ID + "/" + a.ID + "/api"
			}
			found := false
			for _, step := range op.Steps {
				if step.ID == expected && step.Status == "started" {
					found = true
				}
			}
			if !found {
				t.Fatalf("started step missing: %+v", op.Steps)
			}
			targetSel := Selection{p, b, "api"}
			if stage == "stop" {
				targetSel = Selection{p, a, "api"}
			}
			_, recoverErr := r.DownGraph(ctx, []Selection{targetSel}, false)
			if stage == "stop" {
				if recoverErr == nil {
					t.Fatal("interrupted stop hook was accepted")
				}
			} else if recoverErr != nil {
				t.Fatalf("cleanup: %v", recoverErr)
			}
			data, e := os.ReadFile(marker)
			if e != nil || strings.TrimSpace(string(data)) != "invoked" {
				t.Fatalf("effect replayed: %q %v", data, e)
			}
		})
	}
}
