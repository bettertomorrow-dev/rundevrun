//go:build linux

package runner

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
)

// verifySessionGone proves absence, never ownership. A reused session ID or an
// unrecorded survivor causes refusal; this function never signals any process.
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
		return nil // Processes from the recorded OS boot cannot still exist.
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return unknown(err)
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return unknown(err)
		}
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			return unknown(fmt.Errorf("malformed process stat for %d", pid))
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 20 {
			return unknown(fmt.Errorf("short process stat for %d", pid))
		}
		if fields[0] == "Z" || fields[0] == "X" {
			continue // Zombies cannot fork, listen or execute project commands.
		}
		sid, err := strconv.Atoi(fields[3])
		if err != nil {
			return unknown(fmt.Errorf("invalid session ID for %d: %w", pid, err))
		}
		if sid == anchor.SID {
			return unknown(fmt.Errorf("live process %d remains in recorded session %d; cleanup is unconfirmed", pid, sid))
		}
	}
	// The kernel group probe catches a same-group child forked after ReadDir's
	// snapshot, even when its parent exited before we read the parent's stat.
	// Allow a short bounded interval for already-dead children to be reaped.
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
