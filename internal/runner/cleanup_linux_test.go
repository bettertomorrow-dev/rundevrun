//go:build linux

package runner

import (
	"bufio"
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

func TestCleanupRefusesUnrecordedSessionSurvivor(t *testing.T) {
	// Block the session leader until its identity is recorded, then leave a
	// descendant that deliberately never appears in the simulated run record.
	cmd := exec.Command("/bin/sh", "-c", "read go; sleep 60 & echo $!; exit")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	anchor, err := procid.Observe(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(in, "go"); err != nil {
		t.Fatal(err)
	}
	in.Close()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	child, err := procid.Observe(pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { procid.Terminate([]procid.Ref{child}, time.Second) })
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	err = verifySessionGone(anchor)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "OWNERSHIP_UNKNOWN" {
		t.Fatalf("unrecorded survivor reported cleaned: %v", err)
	}
	if ok, err := procid.Check(child); err != nil || !ok {
		t.Fatalf("verification signalled a process: %v %v", ok, err)
	}
	// A stale or forged owner must still never authorize a signal.
	foreign := anchor
	foreign.Identity += "foreign"
	if err := verifySessionGone(foreign); err == nil {
		t.Fatal("live reused session accepted")
	}
	if ok, _ := procid.Check(child); !ok {
		t.Fatal("foreign session was signalled")
	}
	if remaining := procid.Terminate([]procid.Ref{child}, time.Second); len(remaining) != 0 {
		t.Fatalf("fixture cleanup failed: %+v", remaining)
	}
	if err := verifySessionGone(anchor); err != nil {
		t.Fatalf("empty session not recognized: %v", err)
	}
	oldBoot := anchor
	oldBoot.BootID += "previous"
	if err := verifySessionGone(oldBoot); err != nil {
		t.Fatal(err)
	}
}

func TestStopDoesNotConfirmCleanupOfLateFork(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "late-child")
	// A valid but uncooperative supervisor forks after Stop has taken its
	// pre-signal snapshot. The new child is absent from the durable run record.
	cmd := exec.Command("python3", "-u", "-c", `
import os, signal, subprocess, sys
child = subprocess.Popen(["sleep", "60"], preexec_fn=os.setpgrp)
def stop(signum, frame):
    late = subprocess.Popen(["sleep", "60"], preexec_fn=os.setpgrp)
    with open(sys.argv[1], "w") as f:
        f.write(str(late.pid))
    os._exit(0)
signal.signal(signal.SIGTERM, stop)
print(child.pid)
sys.stdin.readline()
`, marker)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		in.Close()
		cmd.Process.Kill()
		cmd.Wait()
		if data, err := os.ReadFile(marker); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				if ref, err := procid.Observe(pid); err == nil {
					procid.Terminate([]procid.Ref{ref}, time.Second)
				}
			}
		}
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	initialChild, err := procid.Observe(childPID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { procid.Terminate([]procid.Ref{initialChild}, time.Second) })
	supervisor, err := procid.Observe(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.New(filepath.Join(dir, "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	const id = "p/w/s"
	if err := store.Update(func(reg *state.Registry) error {
		return reg.SetProject("p", state.Project{ID: "p", Name: "fixture", Root: dir, Worktrees: []state.Worktree{{ID: "w", Root: dir}}, Instances: map[string]state.Instance{id: {ID: id, Service: "s", WorktreeID: "w", State: "starting", RunID: "run", Reservations: map[string]int{"run": 32123}, Intent: &state.Intent{OperationID: "op", RunID: "run", Phase: "spawning"}}}})
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteManifest("run", state.Manifest{RunID: "run", InstanceID: id, OperationID: "op", Launch: state.LaunchContract{StopTimeout: "1s"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteRun("run", state.RunRecord{Claim: "accepted", Supervisor: supervisor, Child: initialChild, State: "starting"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = New(store, "").Stop(ctx, &discovery.Project{ID: "p"}, &discovery.Worktree{ID: "w", Root: dir}, "s")
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "OWNERSHIP_UNKNOWN" {
		t.Fatalf("late child incorrectly reported stopped: %v", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal("fixture did not fork on TERM:", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	child, err := procid.Observe(pid)
	if err != nil {
		t.Fatal("unrecorded child unexpectedly signalled:", err)
	}
	if !procid.Matches(child) {
		t.Fatal("verification killed unrecorded child")
	}
	reg, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	p, err := reg.Project("p")
	if err != nil {
		t.Fatal(err)
	}
	i := p.Instances[id]
	if i.State == "stopped" || i.Reservations["run"] == 0 || i.Intent.Phase == "stopped" {
		t.Fatalf("unconfirmed cleanup cleared state/reservation: %+v", i)
	}
}
