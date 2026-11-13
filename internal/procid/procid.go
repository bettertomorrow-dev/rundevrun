// Adapted from MillDrum internal/procid at commit 1184e35.
// Package procid identifies and stops operating-system processes without
// trusting a bare PID. A process is named by its PID plus the kernel's start
// time, so a recycled PID never matches an old record. Groups are signalled
// only when a verified member still belongs to them.
package procid

import (
	"errors"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Ref names one process the way MillDrum records it.
type Ref struct {
	PID      int
	PGID     int    // process group recorded when the process was observed; 0 = unknown
	SID      int    // login session recorded when the process was observed; 0 = unknown
	Identity string // kernel start time; empty never matches
	BootID   string // kernel boot identity
}

// ErrGone means the process does not exist (or is a zombie).
var ErrGone = errors.New("process does not exist")

// Observe returns a Ref for a live process with its current group.
func Observe(pid int) (Ref, error) {
	id, err := Identity(pid)
	if err != nil {
		return Ref{}, err
	}
	pg, err := syscall.Getpgid(pid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return Ref{}, ErrGone
		}
		return Ref{}, err
	}
	sid, err := unix.Getsid(pid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return Ref{}, ErrGone
		}
		return Ref{}, err
	}
	boot, err := bootID()
	if err != nil {
		return Ref{}, err
	}
	return Ref{PID: pid, PGID: pg, SID: sid, Identity: id, BootID: boot}, nil
}

// Matches reports whether the process recorded in r is alive right now: the
// PID exists, is not a zombie and has the recorded start identity.
func Matches(r Ref) bool { ok, _ := Check(r); return ok }

// Check distinguishes a missing or foreign process from an inspection failure.
func Check(r Ref) (bool, error) {
	if r.PID <= 0 || r.PGID <= 0 || r.SID <= 0 || r.Identity == "" || r.BootID == "" {
		return false, errors.New("incomplete process identity")
	}
	now, err := Observe(r.PID)
	if errors.Is(err, ErrGone) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return now == r, nil
}

// Terminate stops every live process named by refs: SIGTERM, a grace period,
// then SIGKILL. A process group is signalled only when at least one verified,
// still-matching ref currently belongs to that group, so a recycled group ID
// is never hit. It returns the refs that are still alive afterwards.
func Terminate(refs []Ref, grace time.Duration) (alive []Ref) {
	signal := func(sig syscall.Signal) {
		groups := map[int][]Ref{}
		for _, r := range refs {
			ok, err := Check(r)
			if err != nil || !ok {
				continue
			}
			if r.PGID > 1 {
				groups[r.PGID] = append(groups[r.PGID], r)
			}
		}
		for pg, members := range groups {
			for _, r := range members {
				// The kernel can recycle an ID between this check and Kill.
				// No portable atomic check-and-signal primitive exists.
				if ok, err := Check(r); err == nil && ok {
					syscall.Kill(-pg, sig)
					break
				}
			}
		}
		for _, r := range refs {
			if ok, err := Check(r); err == nil && ok {
				syscall.Kill(r.PID, sig)
			}
		}
	}
	live := func() []Ref {
		var out []Ref
		for _, r := range refs {
			ok, err := Check(r)
			if err != nil || ok {
				out = append(out, r)
			}
		}
		return out
	}
	if len(live()) == 0 {
		return nil
	}
	signal(syscall.SIGTERM)
	if waitGone(live, grace) {
		return nil
	}
	signal(syscall.SIGKILL)
	if waitGone(live, 3*time.Second) {
		return nil
	}
	return live()
}

func waitGone(live func() []Ref, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if len(live()) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(15 * time.Millisecond)
	}
}
