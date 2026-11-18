//go:build darwin || linux

// Tests runner dead supervisor behavior.
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

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestDeadSupervisorRecovery(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rundevrun")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	store, err := state.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	const id, run = "p/w/s", "test-run"
	instance := state.Instance{ID: id, Service: "s", WorktreeID: "w", State: "starting", RunID: run, Intent: &state.Intent{RunID: run, OperationID: "op", Phase: "spawning"}}
	if err := store.Update(func(reg *state.Registry) error {
		return reg.SetProject("p", state.Project{ID: "p", Name: "p", Root: dir, Worktrees: []state.Worktree{{ID: "w", Root: dir}}, Instances: map[string]state.Instance{id: instance}})
	}); err != nil {
		t.Fatal(err)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	childFile := filepath.Join(dir, "child-pid")
	manifest := state.Manifest{RunID: run, InstanceID: id, OperationID: "op", Launch: state.LaunchContract{Cwd: dir, Env: append(os.Environ(), "RDR_TEST_SESSION_CHILD="+childFile), Command: strconv.Quote(testBinary) + " -test.run=^TestSupervisedSeparateGroupHelper$", Protocol: "process", StopTimeout: "1s"}}
	if err := store.WriteManifest(run, manifest); err != nil {
		t.Fatal(err)
	}
	supervisor := exec.Command(binary, "__rdr_supervisor", store.Root, run)
	supervisor.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := supervisor.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = supervisor.Process.Kill(); _ = supervisor.Wait() }()
	var record state.RunRecord
	t.Cleanup(func() {
		if record.Child.PID > 0 {
			procid.Terminate(append(record.Members, record.Child), time.Second)
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		record, err = store.ReadRun(run)
		if err == nil && record.State == "running" {
			data, readErr := os.ReadFile(childFile)
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if readErr == nil && parseErr == nil {
				for _, ref := range record.Members {
					if ref.PID == pid && ref.PGID != record.Child.PGID {
						goto ready
					}
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("separate process group not tracked: %+v %v", record, err)
ready:
	foreign := exec.Command("sleep", "60")
	if err := foreign.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = foreign.Process.Kill(); _ = foreign.Wait() }()
	foreignRef, err := procid.Observe(foreign.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err = supervisor.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = supervisor.Wait()
	observed, err := store.Inspect(instance)
	if err != nil || observed != "stale" {
		t.Fatalf("dead supervisor reported %q: %v", observed, err)
	}
	locks, err := store.LockInstances(id)
	if err != nil {
		t.Fatal(err)
	}
	defer locks.Close()
	gate, err := store.LockSpawnGates(run)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	r := New(store, "")
	damaged := record
	damaged.Members = append(append([]procid.Ref(nil), record.Members...), foreignRef)
	if err = store.WriteRun(run, damaged); err != nil {
		t.Fatal(err)
	}
	err = r.stopUnderGate(context.Background(), "p", id, run)
	var ownership *Error
	if !errors.As(err, &ownership) || ownership.Code != "OWNERSHIP_UNKNOWN" {
		t.Fatalf("foreign member accepted: %v", err)
	}
	if ok, err := procid.Check(foreignRef); err != nil || !ok {
		t.Fatalf("foreign process affected: %v %v", ok, err)
	}
	if ok, err := procid.Check(record.Child); err != nil || !ok {
		t.Fatalf("owned process affected on refusal: %v %v", ok, err)
	}
	if err = store.WriteRun(run, record); err != nil {
		t.Fatal(err)
	}
	if err = r.stopUnderGate(context.Background(), "p", id, run); err != nil {
		t.Fatal(err)
	}
	if ok, err := procid.Check(record.Child); err != nil || ok {
		t.Fatalf("child survived: %v %v", ok, err)
	}
	if ok, err := procid.Check(foreignRef); err != nil || !ok {
		t.Fatalf("foreign process affected: %v %v", ok, err)
	}
	reg, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	p, err := reg.Project("p")
	if err != nil {
		t.Fatal(err)
	}
	final := p.Instances[id]
	observed, err = store.Inspect(final)
	if err != nil || observed != "stopped" {
		t.Fatalf("not stopped: %q %v", observed, err)
	}
}

func TestSupervisedSeparateGroupHelper(t *testing.T) {
	path := os.Getenv("RDR_TEST_SESSION_CHILD")
	if path == "" {
		return
	}
	child := exec.Command("sleep", "60")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprint(child.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
}
