package runner

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

// recoverPreparation is called with the instance operation lock held.
func (r *Runner) recoverPreparation(ctx context.Context, projectID, instanceID string) error {
	reg, err := r.Store.Read()
	if err != nil {
		return err
	}
	project, err := reg.Project(projectID)
	if err != nil {
		return err
	}
	instance, ok := project.Instances[instanceID]
	if !ok {
		return os.ErrNotExist
	}
	if instance.Preparation == nil {
		return nil
	}
	runID := instance.Preparation.RunID
	var gate *state.Locks
	for {
		gate, err = r.Store.LockSpawnGates(runID)
		if err == nil {
			break
		}
		if !errors.Is(err, state.ErrBusy) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(state.ErrBusy, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer gate.Close()
	reg, err = r.Store.Read()
	if err != nil {
		return err
	}
	project, err = reg.Project(projectID)
	if err != nil {
		return err
	}
	instance = project.Instances[instanceID]
	if instance.Preparation == nil {
		return nil
	}
	if instance.Preparation.RunID != runID {
		return errors.New("preparation changed")
	}
	record, err := r.Store.ReadPreparation(runID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if errors.Is(err, os.ErrNotExist) {
		manifest, readErr := r.Store.ReadManifest(runID)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		if readErr == nil && (manifest.InstanceID != instanceID || manifest.OperationID != instance.Preparation.OperationID) {
			return &Error{"STATE_CORRUPT", errors.New("preparation manifest mismatch")}
		}
	}
	if err == nil {
		manifest, err := r.Store.ReadManifest(runID)
		if err != nil {
			return err
		}
		if manifest.InstanceID != instanceID || manifest.OperationID != instance.Preparation.OperationID {
			return &Error{"STATE_CORRUPT", errors.New("preparation manifest mismatch")}
		}
		if record.Child.PID <= 0 || record.Child.PGID != record.Child.PID || record.Child.SID != record.Child.PID || record.Child.Identity == "" || record.Child.BootID == "" {
			return &Error{"OWNERSHIP_UNKNOWN", errors.New("invalid preparation identity")}
		}
		refs := append([]procid.Ref{record.Child}, record.Members...)
		for _, ref := range refs {
			if ref.PGID != record.Child.PGID || ref.SID != record.Child.SID || ref.BootID != record.Child.BootID {
				return &Error{"OWNERSHIP_UNKNOWN", errors.New("preparation member outside recorded session")}
			}
		}
		if !record.Complete {
			var anchors []procid.Ref
			for _, ref := range refs {
				live, err := procid.Check(ref)
				if err != nil {
					return &Error{"OWNERSHIP_UNKNOWN", err}
				}
				if live {
					anchors = append(anchors, ref)
				}
			}
			if len(anchors) > 0 {
				members, err := groupMembers(record.Child, anchors)
				if err != nil {
					return &Error{"OWNERSHIP_UNKNOWN", err}
				}
				refs = mergeMembers(refs, members)
				if remaining := procid.Terminate(refs, time.Second); len(remaining) > 0 {
					return &Error{"OWNERSHIP_UNKNOWN", errors.New("build cleanup unresolved")}
				}
			}
		}
		if err = verifySessionGone(record.Child); err != nil {
			return &Error{"OWNERSHIP_UNKNOWN", err}
		}
	}
	return r.Store.Update(func(reg *state.Registry) error {
		project, err := reg.Project(projectID)
		if err != nil {
			return err
		}
		instance, ok := project.Instances[instanceID]
		if !ok {
			return os.ErrNotExist
		}
		if instance.Preparation == nil || instance.Preparation.RunID != runID {
			return errors.New("preparation changed")
		}
		instance.Preparation = nil
		delete(instance.Reservations, runID)
		project.Instances[instanceID] = instance
		return reg.SetProject(projectID, project)
	})
}
