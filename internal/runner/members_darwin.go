//go:build darwin

// Discovers owned process groups and sessions on macOS.
package runner

import (
	"errors"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"golang.org/x/sys/unix"
)

func groupMembers(leader procid.Ref, anchors []procid.Ref) ([]procid.Ref, error) {
	var anchor procid.Ref
	for _, candidate := range anchors {
		if candidate.SID != leader.SID || candidate.BootID != leader.BootID {
			continue
		}
		ok, err := procid.Check(candidate)
		if err != nil {
			return nil, err
		}
		if ok {
			anchor = candidate
			break
		}
	}
	if anchor.PID == 0 {
		return nil, errors.New("no verified live session anchor")
	}
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	var refs []procid.Ref
	for _, p := range processes {
		if p.Proc.P_pid <= 0 || p.Proc.P_stat == 5 || int(p.Eproc.Pgid) != leader.PGID {
			continue
		}
		ref, err := procid.Observe(int(p.Proc.P_pid))
		if errors.Is(err, procid.ErrGone) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if ref.PGID == leader.PGID && ref.SID == leader.SID && ref.BootID == leader.BootID {
			refs = append(refs, ref)
		}
	}
	ok, err := procid.Check(anchor)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("session anchor changed during process inspection")
	}
	return refs, nil
}
