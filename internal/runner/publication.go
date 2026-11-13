package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

// ClosePublication revokes one publication under its gate and verifies cleanup.
// The caller holds operation locks for its pinned instances.
func ClosePublication(ctx context.Context, store *state.Store, id string) error {
	var gate *state.Locks
	var e error
	for {
		gate, e = store.LockPublicationGate(id)
		if e == nil {
			break
		}
		if !errors.Is(e, state.ErrBusy) {
			return e
		}
		select {
		case <-ctx.Done():
			return errors.Join(state.ErrBusy, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer gate.Close()
	setPhase := func(phase string, unclaimed bool) error {
		lock, e := store.LockPublicationRegistry()
		if e != nil {
			return e
		}
		defer lock.Close()
		reg, e := store.ReadPublications()
		if e != nil {
			return e
		}
		p, ok := reg.Publications[id]
		if !ok {
			return os.ErrNotExist
		}
		if p.Phase == "closed" {
			return nil
		}
		if unclaimed && p.Phase != "preparing" {
			return &Error{"OWNERSHIP_UNKNOWN", errors.New("active publication runtime missing")}
		}
		p.Phase = phase
		reg.Publications[id] = p
		reg.Revision++
		return store.WritePublications(reg)
	}
	record, e := store.ReadPublicationRuntime(id)
	if errors.Is(e, os.ErrNotExist) {
		return setPhase("closed", true)
	}
	if e != nil {
		return e
	}
	supervisor := record.Supervisor
	if supervisor.PID <= 1 || supervisor.PGID != supervisor.PID || supervisor.SID != supervisor.PID || supervisor.Identity == "" || supervisor.BootID == "" {
		return &Error{"OWNERSHIP_UNKNOWN", errors.New("invalid publication supervisor identity")}
	}
	refs := append([]procid.Ref(nil), record.Members...)
	if record.Tunnel != (procid.Ref{}) {
		if record.Tunnel.PID <= 1 || record.Tunnel.PGID != record.Tunnel.PID || record.Tunnel.SID != supervisor.SID || record.Tunnel.BootID != supervisor.BootID || record.Tunnel.Identity == "" {
			return &Error{"OWNERSHIP_UNKNOWN", errors.New("invalid tunnel identity")}
		}
		refs = append(refs, record.Tunnel)
	}
	if e = setPhase("closing", false); e != nil {
		return e
	}
	anchors := []procid.Ref{}
	for _, ref := range append([]procid.Ref{supervisor}, refs...) {
		if ref.SID != supervisor.SID || ref.BootID != supervisor.BootID {
			return &Error{"OWNERSHIP_UNKNOWN", errors.New("publication member outside session")}
		}
		live, err := procid.Check(ref)
		if err != nil {
			return &Error{"OWNERSHIP_UNKNOWN", err}
		}
		if live {
			anchors = append(anchors, ref)
		}
	}
	if len(anchors) > 0 && record.Tunnel != (procid.Ref{}) {
		members, err := groupMembers(record.Tunnel, anchors)
		if err != nil {
			return &Error{"OWNERSHIP_UNKNOWN", err}
		}
		refs = mergeMembers(refs, members)
	}
	live, e := procid.Check(supervisor)
	if e != nil {
		return &Error{"OWNERSHIP_UNKNOWN", e}
	}
	if live {
		if e = syscall.Kill(supervisor.PID, syscall.SIGTERM); e != nil && !errors.Is(e, syscall.ESRCH) {
			return e
		}
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			live, e = procid.Check(supervisor)
			if e != nil {
				return &Error{"OWNERSHIP_UNKNOWN", e}
			}
			if !live {
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline.C:
				if len(procid.Terminate([]procid.Ref{supervisor}, 0)) > 0 {
					return &Error{"OWNERSHIP_UNKNOWN", errors.New("publication supervisor cleanup unresolved")}
				}
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	if len(procid.Terminate(refs, time.Second)) > 0 {
		return &Error{"OWNERSHIP_UNKNOWN", errors.New("tunnel cleanup unresolved")}
	}
	if e = verifySessionGone(supervisor); e != nil {
		return &Error{"OWNERSHIP_UNKNOWN", e}
	}
	return setPhase("closed", false)
}

// ClosePublicationsForInstances closes roots and explicitly routed targets before app stop.
// The caller holds locks for all affected instance IDs.
func ClosePublicationsForInstances(ctx context.Context, store *state.Store, ids []string) error {
	selected := map[string]bool{}
	for _, id := range ids {
		selected[id] = true
	}
	reg, e := store.ReadPublications()
	if e != nil {
		return e
	}
	for id, p := range reg.Publications {
		if p.Phase == "closed" {
			continue
		}
		affected := selected[p.RootID]
		for target := range p.RoutedRuns {
			affected = affected || selected[target]
		}
		if affected {
			if e = ClosePublication(ctx, store, id); e != nil {
				return fmt.Errorf("publication %s: %w", id, e)
			}
		}
	}
	return nil
}
