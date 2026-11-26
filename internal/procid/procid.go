// Identifies and stops processes using verified kernel identities.
package procid

import (
	"errors"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type Ref struct {
	PID      int
	PGID     int
	SID      int
	Identity string
	BootID   string
}

var ErrGone = errors.New("process does not exist")

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

func Matches(r Ref) bool { ok, _ := Check(r); return ok }

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
	if now.PGID != r.PGID || now.SID != r.SID || now.Identity != r.Identity {
		return false, nil
	}
	return matchesBootID(r.BootID, now.BootID)
}

func InCurrentBoot(recorded string) (bool, error) {
	current, err := bootID()
	if err != nil {
		return false, err
	}
	return matchesBootID(recorded, current)
}

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
