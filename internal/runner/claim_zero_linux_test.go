//go:build linux

package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestClaimedWithoutChildCwdFailureRecovers(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rdr")
	if b, e := exec.Command("go", "build", "-o", binary, "../../cmd/rdr").CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	root := filepath.Join(dir, "project")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	sub := filepath.Join(root, "sub")
	if e := os.Mkdir(sub, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := `version = 1
[project]
name = 'demo'
[services.app]
command = 'sleep 60'
cwd = 'sub'
start_timeout = '600ms'
hooks = { before_start = 'cd ..; rmdir sub', before_stop = 'touch ../stop-hook-ran', after_stop = 'touch ../stop-hook-ran' }
`
	path := filepath.Join(root, "dev.toml")
	if e := os.WriteFile(path, []byte(cfg), 0600); e != nil {
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
	if e = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: map[string]state.Instance{id: {ID: id, Service: "app", WorktreeID: w.ID, State: "stopped"}}})
	}); e != nil {
		t.Fatal(e)
	}
	r := New(store, binary)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	defer func() {
		reg, e := store.Read()
		if e == nil {
			pr, e := reg.Project(p.ID)
			if e == nil {
				i := pr.Instances[id]
				if i.RunID != "" {
					_ = r.stopLocked(context.Background(), p.ID, id, i.RunID)
				}
			}
		}
	}()
	started := time.Now()
	if _, e = r.Up(ctx, p, w, "app"); e == nil {
		t.Fatal("expected cwd startup failure")
	}
	var processErr *Error
	if !errors.As(e, &processErr) || processErr.Code != "PROCESS_FAILED" || !strings.Contains(e.Error(), "sub") {
		t.Fatalf("expected cwd cause: %v", e)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatalf("startup failure was delayed: %s", time.Since(started))
	}
	reg, e := store.Read()
	if e != nil {
		t.Fatal(e)
	}
	pr, e := reg.Project(p.ID)
	if e != nil {
		t.Fatal(e)
	}
	old := pr.Instances[id]
	record, e := store.ReadRun(old.RunID)
	if e != nil || record.Claim != "accepted" || record.Child != (procid.Ref{}) {
		t.Fatalf("expected zero-child claim: %+v %v", record, e)
	}
	if e = os.Mkdir(sub, 0700); e != nil {
		t.Fatal(e)
	}
	res, e := r.Stop(ctx, p, w, "app")
	if e != nil || res.State != "stopped" {
		t.Fatalf("down: %+v %v", res, e)
	}
	if _, e = os.Stat(filepath.Join(root, "stop-hook-ran")); !os.IsNotExist(e) {
		t.Fatalf("stop hook ran: %v", e)
	}
	reg, e = store.Read()
	if e != nil {
		t.Fatal(e)
	}
	pr, e = reg.Project(p.ID)
	if e != nil {
		t.Fatal(e)
	}
	observed, e := store.Inspect(pr.Instances[id])
	if e != nil || observed != "stopped" {
		t.Fatalf("status: %s %v", observed, e)
	}
	fixed := strings.Replace(cfg, "before_start = 'cd ..; rmdir sub', ", "", 1)
	if e = os.WriteFile(path, []byte(fixed), 0600); e != nil {
		t.Fatal(e)
	}
	newRun, e := r.Up(ctx, p, w, "app")
	if e != nil || newRun.RunID == old.RunID {
		t.Fatalf("new up: %+v %v", newRun, e)
	}
}
func TestClaimedWithoutChildUnknownSurvivor(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "project")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "dev.toml"), []byte("version = 1\n[project]\nname = 'demo'\n[services.app]\ncommand = 'sleep 60'\n"), 0600); e != nil {
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
	// The recorded supervisor exits while a child remains in its session.
	marker := filepath.Join(dir, "child")
	leader := exec.Command("sh", "-c", "sleep 60 & echo $! > "+marker+"; sleep 0.4")
	leader.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if e = leader.Start(); e != nil {
		t.Fatal(e)
	}
	ref, e := procid.Observe(leader.Process.Pid)
	if e != nil {
		t.Fatal(e)
	}
	var b []byte
	for n := 0; n < 100; n++ {
		b, e = os.ReadFile(marker)
		if e == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e != nil {
		t.Fatal(e)
	}
	pid, e := strconv.Atoi(strings.TrimSpace(string(b)))
	if e != nil {
		t.Fatal(e)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	if e = leader.Wait(); e != nil {
		t.Fatal(e)
	}
	m := state.Manifest{RunID: "run", InstanceID: id, OperationID: "op", StopContract: state.StopContract{Cwd: root, RunDir: filepath.Join(store.Root, "runs", "run"), HookTimeout: "1s", Hooks: map[string]string{"before_stop": "touch hook-ran"}}, Launch: state.LaunchContract{StopTimeout: "1s"}}
	if e = store.WriteManifest("run", m); e != nil {
		t.Fatal(e)
	}
	if e = store.WriteRun("run", state.RunRecord{Claim: "accepted", Supervisor: ref, State: "starting"}); e != nil {
		t.Fatal(e)
	}
	if e = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: map[string]state.Instance{id: {ID: id, Service: "app", WorktreeID: w.ID, State: "starting", RunID: "run", Reservations: map[string]int{"run": 23456}, Intent: &state.Intent{RunID: "run", OperationID: "op", Phase: "spawning"}}}})
	}); e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if e != nil {
		t.Fatal(e)
	}
	_, e = New(store, "").Stop(context.Background(), p, w, "app")
	var unknown *Error
	if !errors.As(e, &unknown) || unknown.Code != "OWNERSHIP_UNKNOWN" {
		t.Fatalf("expected unknown ownership: %v", e)
	}
	after, e := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if e != nil || string(before) != string(after) {
		t.Fatal("uncertain session modified")
	}
	if e = syscall.Kill(pid, 0); e != nil {
		t.Fatalf("survivor signalled: %v", e)
	}
	if _, e = os.Stat(filepath.Join(root, "hook-ran")); !os.IsNotExist(e) {
		t.Fatalf("hook ran: %v", e)
	}
	// Partial identities are rejected, also before signalling or changing metadata.
	if e = store.WriteRun("run", state.RunRecord{Claim: "accepted", Supervisor: procid.Ref{PID: ref.PID}, State: "starting"}); e != nil {
		t.Fatal(e)
	}
	_, e = New(store, "").Stop(context.Background(), p, w, "app")
	if !errors.Is(e, state.ErrOwnershipUnknown) {
		t.Fatalf("partial identity accepted: %v", e)
	}
	after, e = os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if e != nil || string(before) != string(after) {
		t.Fatal("partial identity modified state")
	}
	if e = syscall.Kill(pid, 0); e != nil {
		t.Fatal(fmt.Errorf("survivor signalled: %w", e))
	}
	mismatched := ref
	mismatched.BootID = "different-boot"
	if e = store.WriteRun("run", state.RunRecord{Claim: "accepted", Supervisor: mismatched, State: "starting"}); e != nil {
		t.Fatal(e)
	}
	_, e = New(store, "").Stop(context.Background(), p, w, "app")
	if !errors.As(e, &unknown) || unknown.Code != "OWNERSHIP_UNKNOWN" {
		t.Fatalf("boot mismatch accepted: %v", e)
	}
	after, e = os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if e != nil || string(before) != string(after) {
		t.Fatal("boot mismatch modified state")
	}
	if e = syscall.Kill(pid, 0); e != nil {
		t.Fatalf("boot mismatch signalled survivor: %v", e)
	}
}
