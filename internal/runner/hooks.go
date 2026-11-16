// Runs lifecycle hooks and records their recovery state.
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func (r *Runner) hookUpdate(projectID, id string, fn func(*state.Instance) error) error {
	return r.Store.Update(func(reg *state.Registry) error {
		p, e := reg.Project(projectID)
		if e != nil {
			return e
		}
		i, ok := p.Instances[id]
		if !ok {
			return os.ErrNotExist
		}
		if e = fn(&i); e != nil {
			return e
		}
		p.Instances[id] = i
		return reg.SetProject(projectID, p)
	})
}

func (r *Runner) runHook(ctx context.Context, projectID string, manifest state.Manifest, name, reason, oldWorktree, newWorktree, operationID string) error {
	command := manifest.StopContract.Hooks[name]
	if command == "" {
		return nil
	}
	h := &state.HookIntent{RunID: manifest.RunID, OperationID: operationID, Name: name, Phase: "started", Reason: reason, OldWorktree: oldWorktree, NewWorktree: newWorktree}
	if e := r.hookUpdate(projectID, manifest.InstanceID, func(i *state.Instance) error {
		if i.Hook != nil && i.Hook.Phase != "completed" && strings.HasSuffix(i.Hook.Name, "_stop") && !(r.RetryHooks && i.Hook.RunID == manifest.RunID && i.Hook.Name == name) {
			return &Error{"HOOK_FAILED", fmt.Errorf("previous hook %s is %s; explicit retry required", i.Hook.Name, i.Hook.Phase)}
		}
		i.Hook = h
		return nil
	}); e != nil {
		return e
	}
	gate, e := r.Store.LockSpawnGates(manifest.RunID)
	if e != nil {
		return e
	}
	env := make(map[string]string, len(manifest.StopContract.Env)+6)
	for k, v := range manifest.StopContract.Env {
		env[k] = v
	}
	for k, v := range manifest.StopContract.HookEnv[name] {
		env[k] = v
	}
	env["RDR_REASON"] = reason
	env["RDR_INSTANCE_ID"] = manifest.InstanceID
	env["RDR_OLD_WORKTREE"] = oldWorktree
	env["RDR_NEW_WORKTREE"] = newWorktree
	env["RDR_OPERATION_ID"] = operationID
	log, e := openLog(filepath.Join(manifest.StopContract.RunDir, "hook-"+name+".log"))
	if e != nil {
		gate.Close()
		return e
	}
	binary := r.Executable
	if binary == "" {
		binary, e = os.Executable()
	}
	if e != nil {
		log.Close()
		gate.Close()
		return e
	}
	timeout, e := time.ParseDuration(manifest.StopContract.HookTimeout)
	if e != nil {
		log.Close()
		gate.Close()
		return e
	}
	persist := func(child procid.Ref) error {
		return r.hookUpdate(projectID, manifest.InstanceID, func(i *state.Instance) error {
			if i.Hook == nil || i.Hook.OperationID != operationID || i.Hook.Name != name {
				return errors.New("hook journal changed")
			}
			i.Hook.Child = child
			return nil
		})
	}
	persistMembers := func(members []procid.Ref) error {
		return r.hookUpdate(projectID, manifest.InstanceID, func(i *state.Instance) error {
			if i.Hook == nil || i.Hook.OperationID != operationID || i.Hook.Name != name {
				return errors.New("hook journal changed")
			}
			i.Hook.Members = members
			return nil
		})
	}
	hookErr := runBuildCommand(ctx, command, manifest.StopContract.Cwd, mergeEnv(env, nil), log, timeout, binary, persist, gate.Close, persistMembers)
	gate.Close()
	hookErr = errors.Join(hookErr, log.Close())
	phase := "completed"
	if hookErr != nil {
		phase = "failed"
	}
	journalErr := r.hookUpdate(projectID, manifest.InstanceID, func(i *state.Instance) error {
		if i.Hook == nil || i.Hook.OperationID != operationID || i.Hook.Name != name {
			return errors.New("hook journal changed")
		}
		i.Hook.Phase = phase
		return nil
	})
	if hookErr != nil {
		if ctx.Err() == nil && errors.Is(hookErr, context.DeadlineExceeded) {
			hookErr = fmt.Errorf("hook timeout: %v", hookErr)
		}
		return &Error{"HOOK_FAILED", errors.Join(hookErr, journalErr)}
	}
	return journalErr
}

func (r *Runner) recoverHook(ctx context.Context, projectID, id, reason, newWorktree, operationID string) error {
	reg, e := r.Store.Read()
	if e != nil {
		return e
	}
	p, e := reg.Project(projectID)
	if e != nil {
		return e
	}
	i, ok := p.Instances[id]
	if !ok {
		return os.ErrNotExist
	}
	h := i.Hook
	if h == nil || h.Phase == "completed" {
		return nil
	}
	gate, e := r.Store.LockSpawnGates(h.RunID)
	if e != nil {
		return e
	}
	defer gate.Close()
	if e = ctx.Err(); e != nil {
		return e
	}
	if h.Child != (procid.Ref{}) {
		if h.Child.PID <= 0 || h.Child.PGID != h.Child.PID || h.Child.SID != h.Child.PID || h.Child.Identity == "" || h.Child.BootID == "" {
			return &Error{"OWNERSHIP_UNKNOWN", errors.New("invalid hook process identity")}
		}
		refs := append([]procid.Ref{h.Child}, h.Members...)
		var live []procid.Ref
		for _, ref := range refs {
			if ref.PGID != h.Child.PGID || ref.SID != h.Child.SID || ref.BootID != h.Child.BootID {
				return &Error{"OWNERSHIP_UNKNOWN", errors.New("hook member outside session")}
			}
			ok, e := procid.Check(ref)
			if e != nil {
				return &Error{"OWNERSHIP_UNKNOWN", e}
			}
			if ok {
				live = append(live, ref)
			}
		}
		if len(live) > 0 {
			members, e := groupMembers(h.Child, live)
			if e != nil {
				return &Error{"OWNERSHIP_UNKNOWN", e}
			}
			refs = mergeMembers(refs, members)
			if len(procid.Terminate(refs, time.Second)) > 0 {
				return &Error{"OWNERSHIP_UNKNOWN", errors.New("hook cleanup unresolved")}
			}
		}
		if e = verifySessionGone(h.Child); e != nil {
			return &Error{"OWNERSHIP_UNKNOWN", e}
		}
	}
	if h.Phase == "started" {
		if e = r.hookUpdate(projectID, id, func(i *state.Instance) error {
			if i.Hook == nil || i.Hook.OperationID != h.OperationID || i.Hook.Name != h.Name {
				return errors.New("hook journal changed")
			}
			i.Hook.Phase = "interrupted"
			return nil
		}); e != nil {
			return e
		}
		h.Phase = "interrupted"
	}

	if e = gate.Close(); e != nil {
		return e
	}
	return r.resolveHook(ctx, projectID, id, h, reason, newWorktree, operationID)
}

func (r *Runner) stopWithHooks(ctx context.Context, projectID, id, runID, reason, newWorktree, operationID string) error {
	gate, e := r.lockSpawnGate(ctx, runID)
	if e != nil {
		return e
	}
	record, readErr := r.Store.ReadRun(runID)
	if errors.Is(readErr, os.ErrNotExist) {
		err := r.stopUnderGate(ctx, projectID, id, runID)
		return errors.Join(err, gate.Close())
	}
	if readErr != nil {
		gate.Close()
		return readErr
	}
	if record.Claim == "accepted" && record.Child == (procid.Ref{}) {
		err := r.stopUnderGate(ctx, projectID, id, runID)
		return errors.Join(err, gate.Close())
	}
	if record.Claim == "accepted" && (record.Supervisor.PID <= 0 || record.Supervisor.Identity == "" || record.Supervisor.BootID == "") {
		gate.Close()
		return &Error{"OWNERSHIP_UNKNOWN", errors.New("claimed supervisor has no verified identity")}
	}
	if record.Child.PID <= 0 || record.Child.Identity == "" || record.Child.BootID == "" {
		gate.Close()
		return &Error{"OWNERSHIP_UNKNOWN", errors.New("supervisor has no verified child identity")}
	}
	if e = gate.Close(); e != nil {
		return e
	}
	if e = ClosePublicationsForInstances(ctx, r.Store, []string{id}); e != nil {
		return e
	}
	manifest, e := r.Store.ReadManifest(runID)
	if e != nil {
		return e
	}
	if manifest.InstanceID != id {
		return fmt.Errorf("%w: manifest instance mismatch", state.ErrCorrupt)
	}
	reg, e := r.Store.Read()
	if e != nil {
		return e
	}
	project, e := reg.Project(projectID)
	if e != nil {
		return e
	}
	previous := project.Instances[id].Hook
	if previous == nil || previous.RunID != runID || previous.Name != "before_stop" || previous.Phase != "completed" {
		if e = r.runHook(ctx, projectID, manifest, "before_stop", reason, manifest.StopContract.Worktree, newWorktree, operationID); e != nil {
			return e
		}
	}
	if e = r.stopLocked(ctx, projectID, id, runID); e != nil {
		return e
	}
	return r.runHook(ctx, projectID, manifest, "after_stop", reason, manifest.StopContract.Worktree, newWorktree, operationID)
}

func (r *Runner) resolveHook(ctx context.Context, projectID, id string, h *state.HookIntent, reason, newWorktree, operationID string) error {
	if strings.HasSuffix(h.Name, "_start") {
		return nil
	}
	if !r.RetryHooks {
		return &Error{"HOOK_FAILED", fmt.Errorf("hook %s is %s; explicit retry required", h.Name, h.Phase)}
	}
	manifest, e := r.Store.ReadManifest(h.RunID)
	if e != nil {
		return e
	}
	if manifest.InstanceID != id {
		return fmt.Errorf("%w: hook manifest mismatch", state.ErrCorrupt)
	}
	return r.runHook(ctx, projectID, manifest, h.Name, reason, manifest.StopContract.Worktree, newWorktree, operationID)
}
