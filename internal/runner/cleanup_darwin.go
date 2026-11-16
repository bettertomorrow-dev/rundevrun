//go:build darwin

// Verifies and cleans up owned processes on macOS.
package runner

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"golang.org/x/sys/unix"
)

func verifySessionGone(anchor procid.Ref) error {
	unknown := func(err error) error { return &Error{Code: "OWNERSHIP_UNKNOWN", Err: err} }
	if anchor.PID <= 0 || anchor.PGID <= 0 || anchor.SID <= 1 || anchor.Identity == "" || anchor.BootID == "" {
		return unknown(errors.New("cannot verify cleanup without recorded session identity"))
	}
	self, err := procid.Observe(os.Getpid())
	if err != nil {
		return unknown(err)
	}
	if self.BootID != anchor.BootID {
		return nil
	}
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return unknown(err)
	}
	for _, p := range processes {
		if p.Proc.P_pid <= 0 || p.Proc.P_stat == 5 {
			continue
		}
		sid, err := unix.Getsid(int(p.Proc.P_pid))
		if errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return unknown(err)
		}
		if sid == anchor.SID {
			return unknown(fmt.Errorf("live process %d remains in recorded session %d; cleanup is unconfirmed", p.Proc.P_pid, sid))
		}
	}
	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		err := syscall.Kill(-anchor.PGID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return unknown(fmt.Errorf("cannot verify recorded process group: %w", err))
		}
		if !time.Now().Before(deadline) {
			return unknown(fmt.Errorf("recorded process group %d still exists; cleanup is unconfirmed", anchor.PGID))
		}
		time.Sleep(10 * time.Millisecond)
	}
}
