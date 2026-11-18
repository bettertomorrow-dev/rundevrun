//go:build darwin

// Discovers owned process groups and sessions on macOS.
package runner

import (
	"errors"
	"syscall"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"golang.org/x/sys/unix"
)

func groupMembers(leader procid.Ref, anchors []procid.Ref) ([]procid.Ref, error) {
	return scanMembers(leader, anchors, false)
}

func runtimeMembers(leader, supervisor procid.Ref) ([]procid.Ref, error) {
	if supervisor.PID <= 1 || supervisor.PGID != supervisor.PID || supervisor.SID != supervisor.PID || supervisor.Identity == "" || supervisor.BootID == "" || leader.SID != supervisor.SID || leader.BootID != supervisor.BootID {
		return nil, errors.New("invalid detached supervisor session")
	}
	return scanMembers(leader, []procid.Ref{supervisor}, true)
}

func scanMembers(leader procid.Ref, anchors []procid.Ref, session bool) ([]procid.Ref, error) {
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
		if p.Proc.P_pid <= 0 || p.Proc.P_stat == 5 || (!session && int(p.Eproc.Pgid) != leader.PGID) || (session && int(p.Proc.P_pid) == anchor.PID) {
			continue
		}
		if session {
			sid, err := unix.Getsid(int(p.Proc.P_pid))
			if errors.Is(err, syscall.ESRCH) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if sid != leader.SID {
				continue
			}
		}
		ref, err := procid.Observe(int(p.Proc.P_pid))
		if errors.Is(err, procid.ErrGone) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if (session || ref.PGID == leader.PGID) && ref.SID == leader.SID && ref.BootID == leader.BootID {
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
