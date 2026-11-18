//go:build linux

// Tests runner supervisor behavior on Linux.
package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestSupervisorRealBinary(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rundevrun")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v: %s", err, out)
	}
	store, err := state.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	id := "p/w/s"
	const run = "test-run"
	if err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject("p", state.Project{ID: "p", Name: "p", Root: dir, Worktrees: []state.Worktree{{ID: "w", Root: dir}}, Instances: map[string]state.Instance{id: {ID: id, Service: "s", WorktreeID: "w", State: "starting", RunID: run, Intent: &state.Intent{RunID: run, OperationID: "op", Phase: "spawning"}}}})
	}); err != nil {
		t.Fatal(err)
	}
	m := state.Manifest{RunID: run, InstanceID: id, OperationID: "op", Launch: state.LaunchContract{Cwd: dir, Env: os.Environ(), Command: "sleep 60", Protocol: "process", StopTimeout: "1s"}}
	if err = store.WriteManifest(run, m); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "__rdr_supervisor", store.Root, run)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	var record state.RunRecord
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		record, err = store.ReadRun(run)
		if err == nil && record.State == "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || record.State != "running" {
		t.Fatalf("not running: %+v %v", record, err)
	}
	if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatalf("supervisor: %v", err)
	}
	record, err = store.ReadRun(run)
	if err != nil || record.State != "stopped" {
		t.Fatalf("not stopped: %+v %v", record, err)
	}
	for _, ref := range record.Members {
		if ok, err := procid.Check(ref); err != nil || ok {
			t.Fatalf("member remains: %+v %v", ref, err)
		}
	}
}
