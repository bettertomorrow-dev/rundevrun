//go:build linux

// Discovers owned process groups and sessions on Linux.
package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
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
	valid := false
	var verifiedAnchor procid.Ref
	for _, anchor := range anchors {
		if anchor.SID != leader.SID || anchor.BootID != leader.BootID {
			continue
		}
		ok, err := procid.Check(anchor)
		if err != nil {
			return nil, err
		}
		if ok {
			valid = true
			verifiedAnchor = anchor
			break
		}
	}
	if !valid {
		return nil, errors.New("no verified live session anchor")
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var refs []procid.Ref
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, err
		}
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			return nil, fmt.Errorf("malformed proc stat for %d", pid)
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 4 {
			return nil, fmt.Errorf("short proc stat for %d", pid)
		}
		pg, e1 := strconv.Atoi(fields[2])
		sid, e2 := strconv.Atoi(fields[3])
		if e1 != nil || e2 != nil {
			return nil, fmt.Errorf("invalid proc stat for %d", pid)
		}
		if sid != leader.SID || (!session && pg != leader.PGID) || (session && pid == verifiedAnchor.PID) {
			continue
		}
		ref, err := procid.Observe(pid)
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
	if session {
		ok, err := procid.Check(verifiedAnchor)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("session anchor changed during process inspection")
		}
	}
	return refs, nil
}
