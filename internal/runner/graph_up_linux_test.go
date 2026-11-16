//go:build linux

// Tests runner graph up behavior on Linux.
package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestUpGraphSharedDependency(t *testing.T) {
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
	if _, e = r.UpGraph(ctx, []Selection{{p, a, "worker"}, {p, b, "missing"}}); e == nil {
		t.Fatal("unknown selection accepted")
	}
	if _, e = os.Stat(filepath.Join(store.Root, "runs")); !os.IsNotExist(e) {
		t.Fatalf("invalid selection ran anything: %v", e)
	}
	result, e := r.UpGraph(ctx, []Selection{{p, a, "worker"}, {p, b, "worker"}, {p, d, "worker"}})
	if e != nil {
		t.Fatalf("up: %+v %v", result, e)
	}
	if len(result.Results) != 5 {
		t.Fatalf("unexpected results: %+v", result)
	}
	port := result.Results[0].URLs["port"]
	if port == "" {
		t.Fatal("missing port")
	}
	for _, w := range []*discovery.Worktree{a, b} {
		for _, f := range []string{"built", "started"} {
			var data []byte
			for n := 0; n < 40; n++ {
				data, e = os.ReadFile(filepath.Join(w.Root, f))
				if e == nil {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			if e != nil || string(data) != port {
				t.Fatalf("%s %s: %q %v", w.Root, f, data, e)
			}
		}
	}
	if _, e = r.Stop(ctx, p, a, "api"); e == nil {
		t.Fatal("stop ignored live consumers")
	}
	failed, err := r.UpGraph(ctx, []Selection{{p, a, "broken"}})
	if err == nil || failed.Status != "partial" {
		t.Fatalf("expected build failure: %+v %v", failed, err)
	}
	var buildPath *os.PathError
	if !errors.As(err, &buildPath) || filepath.Base(buildPath.Path) != "build.log" {
		t.Fatalf("failed build omitted its log path: %v", err)
	}
	if _, err := os.Stat(buildPath.Path); err != nil {
		t.Fatalf("failed build log unavailable: %v", err)
	}
	again, e := r.UpGraph(ctx, []Selection{{p, a, "worker"}, {p, b, "worker"}, {p, d, "worker"}})
	if e != nil || len(again.Results) != 5 {
		t.Fatalf("repeat: %+v %v", again, e)
	}
	concurrent := make(chan error, 2)
	for n := 0; n < 2; n++ {
		go func() {
			_, err := r.UpGraph(ctx, []Selection{{p, a, "worker"}, {p, b, "worker"}, {p, d, "worker"}})
			concurrent <- err
		}()
	}
	for n := 0; n < 2; n++ {
		err := <-concurrent
		var coded *Error
		if err != nil && (!errors.As(err, &coded) || coded.Code != "BUSY") {
			t.Fatalf("concurrent up: %v", err)
		}
	}
	for i := range result.Results {
		if result.Results[i].RunID != again.Results[i].RunID {
			t.Fatal("duplicate run")
		}
	}
}
