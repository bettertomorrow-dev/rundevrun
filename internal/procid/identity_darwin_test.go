//go:build darwin

// Tests procid identity behavior on macOS.
package procid

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReapedProcessIsGone(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ref, err := Observe(cmd.Process.Pid)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Identity(ref.PID); !errors.Is(err, ErrGone) {
		t.Fatalf("reaped process identity: %v, want ErrGone", err)
	}
	if live, err := Check(ref); live || err != nil {
		t.Fatalf("reaped process check: live=%t err=%v", live, err)
	}
}

func TestIdentityReapedProcess(t *testing.T) {
	cmd := exec.Command("/usr/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := Identity(cmd.Process.Pid); !errors.Is(err, ErrGone) {
		t.Fatalf("Identity(reaped pid %d) = %v, want ErrGone", cmd.Process.Pid, err)
	}
}

func TestIdentityLiveProcess(t *testing.T) {
	ref, err := Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := Check(ref); err != nil || !ok {
		t.Fatalf("Check(current process) = %v, %v", ok, err)
	}
}

func TestLegacyBootTimeDrift(t *testing.T) {
	current, err := Observe(os.Getpid())
	if err != nil || current.BootID == "" || LegacyBootID(current.BootID) {
		t.Fatalf("expected a boot-session UUID: %+v %v", current, err)
	}
	boot, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		t.Fatal(err)
	}
	legacy := current
	legacy.BootID = fmt.Sprintf("%d.%06d", boot.Sec-2, boot.Usec)
	if !Matches(legacy) {
		t.Fatal("clock drift invalidated a live legacy reference")
	}
	legacy.Identity += "0"
	if Matches(legacy) {
		t.Fatal("legacy boot time matched a different process identity")
	}
	legacy = current
	legacy.BootID = fmt.Sprintf("%d.%06d", boot.Sec-120, boot.Usec)
	if Matches(legacy) {
		t.Fatal("legacy reference from a different boot matched")
	}
}
