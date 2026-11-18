//go:build linux

// Tests service plan refresh, restart rebuilding, and stale process recovery on Linux.
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
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestMissingSupervisorStoppedAndUncertain(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rundevrun")
	if b, e := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun").CombinedOutput(); e != nil {
		t.Fatalf("build %v %s", e, b)
	}
	root := filepath.Join(dir, "project")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := "version = 1\n[project]\nname = 'demo'\n[services.app]\ncommand = 'sleep 60'\n"
	if e := os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	p, e := discovery.Discover(root)
	if e != nil {
		t.Fatal(e)
	}
	w := &p.Worktrees[0]
	id := p.ID + "/" + w.ID + "/app"
	store, e := state.New(filepath.Join(dir, "state"))
	if e != nil {
		t.Fatal(e)
	}
	if e = store.WriteManifest("unclaimed", state.Manifest{RunID: "unclaimed", InstanceID: id, OperationID: "old", StopContract: state.StopContract{Cwd: root, RunDir: filepath.Join(store.Root, "runs", "unclaimed"), HookTimeout: "1s", Hooks: map[string]string{"before_stop": "touch hooks-ran", "after_stop": "touch hooks-ran"}}, Launch: state.LaunchContract{StopTimeout: "1s"}}); e != nil {
		t.Fatal(e)
	}
	inst := state.Instance{ID: id, Service: "app", WorktreeID: w.ID, State: "starting", RunID: "unclaimed", Intent: &state.Intent{OperationID: "old", RunID: "unclaimed", Phase: "spawning"}}
	if e = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: map[string]state.Instance{id: inst}})
	}); e != nil {
		t.Fatal(e)
	}
	r := New(store, binary)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	gate, e := store.LockSpawnGates("unclaimed")
	if e != nil {
		t.Fatal(e)
	}
	beforeGate, e := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if e != nil {
		t.Fatal(e)
	}
	blockedCtx, blockedCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	_, blockedErr := r.Stop(blockedCtx, p, w, "app")
	blockedCancel()
	if !errors.Is(blockedErr, state.ErrBusy) || !errors.Is(blockedErr, context.DeadlineExceeded) {
		gate.Close()
		t.Fatalf("expected busy/cancel: %v", blockedErr)
	}
	afterGate, e := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if e != nil || string(beforeGate) != string(afterGate) {
		gate.Close()
		t.Fatal("gate-busy stop modified state")
	}
	if _, err := os.Stat(filepath.Join(root, "hooks-ran")); !os.IsNotExist(err) {
		gate.Close()
		t.Fatalf("gate-busy stop ran hook: %v", err)
	}
	if e = gate.Close(); e != nil {
		t.Fatal(e)
	}
	result, e := r.Stop(ctx, p, w, "app")
	if e != nil || result.State != "stopped" {
		t.Fatalf("revoke: %+v %v", result, e)
	}
	if _, err := os.Stat(filepath.Join(root, "hooks-ran")); !os.IsNotExist(err) {
		t.Fatalf("unclaimed stop ran hook: %v", err)
	}
	late := exec.Command(binary, "__rdr_supervisor", store.Root, "unclaimed")
	if out, err := late.CombinedOutput(); err == nil {
		t.Fatalf("late supervisor accepted revoked intent: %s", out)
	}
	result, e = r.Up(ctx, p, w, "app")
	if e != nil || result.RunID == "unclaimed" {
		t.Fatalf("subsequent up: %+v %v", result, e)
	}
	if _, e = r.Stop(ctx, p, w, "app"); e != nil {
		t.Fatal(e)
	}
	ref, e := procid.Observe(os.Getpid())
	if e != nil {
		t.Fatal(e)
	}
	if e = store.WriteManifest("claimed", state.Manifest{RunID: "claimed", InstanceID: id, OperationID: "op-claimed", StopContract: state.StopContract{Cwd: root, RunDir: filepath.Join(store.Root, "runs", "claimed"), HookTimeout: "1s", Hooks: map[string]string{"before_stop": "touch hooks-ran"}}, Launch: state.LaunchContract{StopTimeout: "1s"}}); e != nil {
		t.Fatal(e)
	}
	if e = store.WriteRun("claimed", state.RunRecord{Claim: "accepted", Supervisor: ref, State: "starting"}); e != nil {
		t.Fatal(e)
	}
	if e = store.Update(func(reg *state.Registry) error {
		project, err := reg.Project(p.ID)
		if err != nil {
			return err
		}
		i := project.Instances[id]
		i.State = "starting"
		i.RunID = "claimed"
		i.Intent = &state.Intent{RunID: "claimed", OperationID: "op-claimed", Phase: "spawning"}
		i.Reservations = map[string]int{"claimed": 23457}
		project.Instances[id] = i
		return reg.SetProject(p.ID, project)
	}); e != nil {
		t.Fatal(e)
	}
	claimedBefore, e := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if e != nil {
		t.Fatal(e)
	}
	if _, err := r.Stop(ctx, p, w, "app"); err == nil {
		t.Fatal("claimed supervisor without child accepted")
	}
	claimedAfter, e := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if e != nil || string(claimedBefore) != string(claimedAfter) {
		t.Fatal("uncertain claim was modified")
	}
	if _, err := os.Stat(filepath.Join(root, "hooks-ran")); !os.IsNotExist(err) {
		t.Fatalf("uncertain claim ran hook: %v", err)
	}
	if e = store.WriteManifest("uncertain", state.Manifest{RunID: "uncertain", InstanceID: id, OperationID: "old2", Launch: state.LaunchContract{StopTimeout: "1s"}}); e != nil {
		t.Fatal(e)
	}
	if e = store.Update(func(reg *state.Registry) error {
		project, e := reg.Project(p.ID)
		if e != nil {
			return e
		}
		i := project.Instances[id]
		i.State = "running"
		i.RunID = "uncertain"
		i.Intent = &state.Intent{OperationID: "old2", RunID: "uncertain", Phase: "started"}
		i.Reservations = map[string]int{"uncertain": 23456}
		project.Instances[id] = i
		return reg.SetProject(p.ID, project)
	}); e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Stop(ctx, p, w, "app"); !errors.Is(e, state.ErrOwnershipUnknown) {
		t.Fatalf("expected ownership unknown: %v", e)
	}
	after, e := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if e != nil || string(before) != string(after) {
		t.Fatal("uncertain run was modified")
	}
	locks, e := store.LockInstances(id)
	if e != nil {
		t.Fatal(e)
	}
	err := r.stopLocked(ctx, p.ID, id, "uncertain")
	locks.Close()
	var ownership *Error
	if !errors.As(err, &ownership) || ownership.Code != "OWNERSHIP_UNKNOWN" {
		t.Fatalf("stopLocked accepted uncertain run: %v", err)
	}
	after, e = os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if e != nil || string(before) != string(after) {
		t.Fatal("stopLocked modified uncertain run")
	}

}
func TestRetryHooksNeverReplaysUnchangedDependency(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rundevrun")
	if b, e := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun").CombinedOutput(); e != nil {
		t.Fatalf("build %v %s", e, b)
	}
	root := filepath.Join(dir, "project")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := `version = 1
[project]
name = 'demo'
[services.api]
command = 'sleep 60'
hooks = { before_stop = 'echo hit >> hits; exit 1' }
[services.app]
command = 'sleep 60'
depends_on = ['api']
`
	if e := os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	p, e := discovery.Discover(root)
	if e != nil {
		t.Fatal(e)
	}
	w := &p.Worktrees[0]
	store, e := state.New(filepath.Join(dir, "state"))
	if e != nil {
		t.Fatal(e)
	}
	instances := map[string]state.Instance{}
	for _, name := range []string{"api", "app"} {
		id := p.ID + "/" + w.ID + "/" + name
		instances[id] = state.Instance{ID: id, Service: name, WorktreeID: w.ID, State: "stopped"}
	}
	if e = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: instances})
	}); e != nil {
		t.Fatal(e)
	}
	r := New(store, binary)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	defer func() {
		reg, e := store.Read()
		if e != nil {
			return
		}
		project, e := reg.Project(p.ID)
		if e != nil {
			return
		}
		for id, i := range project.Instances {
			if i.RunID != "" {
				_ = r.stopLocked(context.Background(), p.ID, id, i.RunID)
			}
		}
	}()
	if _, e = r.Up(ctx, p, w, "app"); e != nil {
		t.Fatal(e)
	}
	apiID := p.ID + "/" + w.ID + "/api"
	reg, e := store.Read()
	if e != nil {
		t.Fatal(e)
	}
	project, e := reg.Project(p.ID)
	if e != nil {
		t.Fatal(e)
	}
	api := project.Instances[apiID]
	manifest, e := store.ReadManifest(api.RunID)
	if e != nil {
		t.Fatal(e)
	}
	locks, e := store.LockInstances(apiID)
	if e != nil {
		t.Fatal(e)
	}
	e = r.runHook(ctx, p.ID, manifest, "before_stop", "down", root, "", "test-op")
	locks.Close()
	if e == nil {
		t.Fatal("expected hook failure")
	}
	r.RetryHooks = true
	for _, action := range []string{"up-root", "restart", "up-consumer"} {
		if action == "up-consumer" {
			if _, err := r.Stop(ctx, p, w, "app"); err != nil {
				t.Fatal(err)
			}
		}
		if action == "up-root" {
			_, e = r.Up(ctx, p, w, "api")
		} else if action == "restart" {
			_, e = r.Restart(ctx, p, w, "app")
		} else {
			_, e = r.Up(ctx, p, w, "app")
		}
		if e == nil {
			t.Fatalf("%s ignored failed hook", action)
		}
		hits, err := os.ReadFile(filepath.Join(root, "hits"))
		if err != nil || string(hits) != "hit\n" {
			t.Fatalf("%s replayed hook: %q %v", action, hits, err)
		}
		current, err := store.Read()
		if err != nil {
			t.Fatal(err)
		}
		pr, err := current.Project(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if pr.Instances[apiID].RunID != api.RunID {
			t.Fatal("dependency run changed")
		}
		observed, err := store.Inspect(pr.Instances[apiID])
		if err != nil || observed != "running" {
			t.Fatalf("dependency: %s %v", observed, err)
		}
	}
}
