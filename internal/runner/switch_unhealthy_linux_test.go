//go:build linux

package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestSwitchGraphReplacesUnhealthySource(t *testing.T) {
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
stop_timeout = '1s'
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
	defer func() {
		for _, s := range []Selection{{p, a, "worker"}, {p, b, "worker"}, {p, b, "api"}} {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = r.Stop(c, s.Project, s.Worktree, s.Service)
			cancel()
		}
	}()
	sourceID := p.ID + "/" + a.ID + "/api"
	rec, e := store.ReadRun(ids[sourceID])
	if e != nil {
		t.Fatal(e)
	}
	if e = syscall.Kill(rec.Supervisor.PID, syscall.SIGSTOP); e != nil {
		t.Fatal(e)
	}
	defer syscall.Kill(rec.Supervisor.PID, syscall.SIGCONT)
	stopped := false
	for n := 0; n < 100; n++ {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", rec.Supervisor.PID))
		if err == nil && strings.Contains(string(raw), "State:\tT") {
			stopped = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !stopped {
		t.Fatal("supervisor did not pause")
	}
	rec.State = "unhealthy"
	if e = store.WriteRun(ids[sourceID], rec); e != nil {
		t.Fatal(e)
	}
	result, e := r.SwitchGraph(ctx, []SwitchMapping{{From: Selection{p, a, "api"}, To: Selection{p, b, "api"}}})
	if e != nil {
		t.Fatalf("switch: %+v %v", result, e)
	}
	if result.Status != "ok" || result.OperationID == "" || result.JournalPath == "" {
		t.Fatalf("result: %+v", result)
	}
	journal, e := store.ReadOperation(result.OperationID)
	if e != nil || journal.Status != "completed" {
		t.Fatalf("journal: %+v %v", journal, e)
	}
	for _, step := range result.Steps {
		if step.Status != "completed" {
			t.Fatalf("unfinished step: %+v", step)
		}
	}
	reg, e := store.Read()
	if e != nil {
		t.Fatal(e)
	}
	project, e := reg.Project(p.ID)
	if e != nil {
		t.Fatal(e)
	}
	apiA := p.ID + "/" + a.ID + "/api"
	apiB := p.ID + "/" + b.ID + "/api"
	if project.Instances[apiA].State != "stopped" || project.Instances[apiB].RunID == "" {
		t.Fatal("switch endpoints incorrect")
	}
	for _, w := range []*discovery.Worktree{a, b} {
		id := p.ID + "/" + w.ID + "/worker"
		inst := project.Instances[id]
		if inst.RunID == ids[id] || inst.EffectiveBindings["api"] != apiB || inst.DesiredBindings["api"] != apiB {
			t.Fatalf("consumer %s: %+v", id, inst)
		}
	}
	independent := p.ID + "/" + d.ID + "/worker"
	if project.Instances[independent].RunID != ids[independent] {
		t.Fatal("independent process changed")
	}
}
