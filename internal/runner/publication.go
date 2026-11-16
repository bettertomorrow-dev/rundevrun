// Revokes publications before stopping their service instances.
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"syscall"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

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
	for _, child := range []procid.Ref{record.Tunnel} {
		if child == (procid.Ref{}) {
			continue
		}
		if child.PID <= 1 || child.PGID != child.PID || child.SID != supervisor.SID || child.BootID != supervisor.BootID || child.Identity == "" {
			return &Error{"OWNERSHIP_UNKNOWN", errors.New("invalid publication child identity")}
		}
		refs = append(refs, child)
	}
	if record.Build != (procid.Ref{}) {
		b := record.Build
		if b.PID <= 1 || b.PGID != b.PID || b.SID != b.PID || b.Identity == "" || b.BootID == "" || b.BootID != supervisor.BootID {
			return &Error{"OWNERSHIP_UNKNOWN", errors.New("invalid publication build identity")}
		}
		for _, member := range record.BuildMembers {
			if member.SID != b.SID || member.BootID != b.BootID {
				return &Error{"OWNERSHIP_UNKNOWN", errors.New("invalid publication build member")}
			}
		}
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
	for _, child := range []procid.Ref{record.Tunnel} {
		if len(anchors) == 0 || child == (procid.Ref{}) {
			continue
		}
		members, err := groupMembers(child, anchors)
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
	if record.Build != (procid.Ref{}) {
		buildRefs := append([]procid.Ref{record.Build}, record.BuildMembers...)
		var buildAnchors []procid.Ref
		for _, ref := range buildRefs {
			live, err := procid.Check(ref)
			if err != nil {
				return &Error{"OWNERSHIP_UNKNOWN", err}
			}
			if live {
				buildAnchors = append(buildAnchors, ref)
			}
		}
		if len(buildAnchors) != 0 {
			members, err := groupMembers(record.Build, buildAnchors)
			if err != nil {
				return &Error{"OWNERSHIP_UNKNOWN", err}
			}
			buildRefs = mergeMembers(buildRefs, members)
		}
		if len(procid.Terminate(buildRefs, time.Second)) > 0 {
			return &Error{"OWNERSHIP_UNKNOWN", errors.New("publication build cleanup unresolved")}
		}
		if err := verifySessionGone(record.Build); err != nil {
			return &Error{"OWNERSHIP_UNKNOWN", err}
		}
	}
	if e = verifySessionGone(supervisor); e != nil {
		return &Error{"OWNERSHIP_UNKNOWN", e}
	}
	return setPhase("closed", false)
}

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
		if p.Phase == "preparing" {
			for target := range p.BuildRuns {
				affected = affected || selected[target]
			}
		}
		if affected {
			if e = ClosePublication(ctx, store, id); e != nil {
				return fmt.Errorf("publication %s: %w", id, e)
			}
		}
	}
	return nil
}

func publicationIDsForInstances(store *state.Store, ids []string) ([]string, error) {
	selected := map[string]bool{}
	for _, id := range ids {
		selected[id] = true
	}
	reg, err := store.ReadPublications()
	if err != nil {
		return nil, err
	}
	var out []string
	for id, p := range reg.Publications {
		if p.Phase == "closed" {
			continue
		}
		affected := selected[p.RootID]
		for target := range p.RoutedRuns {
			affected = affected || selected[target]
		}
		if p.Phase == "preparing" {
			for target := range p.BuildRuns {
				affected = affected || selected[target]
			}
		}
		if affected {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}
