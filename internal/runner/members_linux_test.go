//go:build linux

// Tests runner members behavior on Linux.
package runner

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
)

func TestGroupMembersAfterLeaderExit(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 60 & exit")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	leader, err := procid.Observe(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := procid.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	refs, err := groupMembers(leader, []procid.Ref{anchor})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) == 0 {
		t.Fatal("lost child after leader exit")
	}
	if remaining := procid.Terminate(refs, time.Second); len(remaining) != 0 {
		t.Fatalf("cleanup: %+v", remaining)
	}
}
