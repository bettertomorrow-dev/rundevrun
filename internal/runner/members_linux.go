//go:build linux

package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
)

// groupMembers snapshots the original session and group, including surviving
// descendants when the original leader has already exited.
func groupMembers(leader procid.Ref, anchors []procid.Ref) ([]procid.Ref, error) {
	valid := false
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
		if errors.Is(err, os.ErrNotExist) {
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
		if pg != leader.PGID || sid != leader.SID {
			continue
		}
		ref, err := procid.Observe(pid)
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
	return refs, nil
}
