//go:build darwin

package procid

import (
	"errors"
	"os/exec"
	"testing"
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
