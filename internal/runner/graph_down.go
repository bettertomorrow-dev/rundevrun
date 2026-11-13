package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

// DownGraph stops selected instances and, when requested, their live consumers.
func (r *Runner) DownGraph(ctx context.Context, selections []Selection, cascade bool) (out GraphResult, retErr error) {
	out = GraphResult{Status: "ok", Results: []Result{}, Errors: []GraphError{}}
	defer func() {
		if retErr != nil {
			out.Status = "error"
			for _, v := range out.Results {
				if v.Status == "completed" {
					out.Status = "partial"
					break
				}
			}
			if len(out.Errors) == 0 {
				code := graphErrorCode(retErr)
				out.Errors = append(out.Errors, GraphError{Code: code, Message: retErr.Error()})
			}
		}
	}()
	initial, err := r.PlanGraph(ctx, "down", selections, cascade)
	if err != nil {
		return out, err
	}
	ids := make([]string, len(initial.Instances))
	expected := map[string]bool{}
	for i, p := range initial.Instances {
		ids[i] = p.InstanceID
		expected[p.InstanceID] = true
	}
	locks, err := r.Store.LockInstances(ids...)
	if err != nil {
		if errors.Is(err, state.ErrBusy) {
			return out, &Error{"BUSY", err}
		}
		return out, err
	}
	defer locks.Close()
	plan, err := r.PlanGraph(ctx, "down", selections, cascade)
	if err != nil {
		return out, err
	}
	if len(plan.Instances) != len(ids) {
		return out, &Error{"STATE_CONFLICT", errors.New("stop graph changed after locking")}
	}
	for _, p := range plan.Instances {
		if !expected[p.InstanceID] {
			return out, &Error{"STATE_CONFLICT", errors.New("stop graph changed after locking")}
		}
	}
	// Capture preparation edges before cleanup so a failed cleanup blocks targets.
	edges := map[string][]string{}
	reg, err := r.Store.Read()
	if err != nil {
		return out, err
	}
	for _, p := range plan.Instances {
		for _, id := range p.Bindings {
			if expected[id] {
				edges[p.InstanceID] = append(edges[p.InstanceID], id)
			}
		}
		parts := strings.Split(p.InstanceID, "/")
		project, e := reg.Project(parts[0])
		if e != nil {
			return out, e
		}
		inst := project.Instances[p.InstanceID]
		if inst.Preparation != nil {
			m, e := r.Store.ReadManifest(inst.Preparation.RunID)
			if e != nil && !errors.Is(e, os.ErrNotExist) {
				return out, e
			}
			if e == nil {
				if m.InstanceID != inst.ID {
					return out, fmt.Errorf("%w: preparation manifest mismatch", state.ErrCorrupt)
				}
				for _, id := range m.EffectiveBindings {
					if expected[id] {
						edges[p.InstanceID] = append(edges[p.InstanceID], id)
					}
				}
			} else {
				// No manifest means targets are unknown. Recover before any stop.
				if e := r.recoverPreparation(ctx, parts[0], inst.ID); e != nil {
					return out, e
				}
			}
		}
	}
	// Preparation bindings can add edges to selected nodes, so include them in stop order.
	plans := map[string]PlannedInstance{}
	for _, p := range plan.Instances {
		plans[p.InstanceID] = p
	}
	marks := map[string]int{}
	var ordered []PlannedInstance
	var visit func(string) error
	visit = func(id string) error {
		if marks[id] == 1 {
			return &Error{"CYCLE", errors.New("effective stop graph contains a cycle")}
		}
		if marks[id] == 2 {
			return nil
		}
		marks[id] = 1
		deps := append([]string(nil), edges[id]...)
		sort.Strings(deps)
		for _, dep := range deps {
			if e := visit(dep); e != nil {
				return e
			}
		}
		marks[id] = 2
		ordered = append(ordered, plans[id])
		return nil
	}
	for _, p := range plan.Instances {
		if e := visit(p.InstanceID); e != nil {
			return out, e
		}
	}
	for i, j := 0, len(ordered)-1; i < j; i, j = i+1, j-1 {
		ordered[i], ordered[j] = ordered[j], ordered[i]
	}
	plan.Instances = ordered
	blocked := map[string]bool{}
	var block func(string)
	block = func(id string) {
		for _, dep := range edges[id] {
			if !blocked[dep] {
				blocked[dep] = true
				block(dep)
			}
		}
	}
	for _, p := range plan.Instances {
		id := p.InstanceID
		parts := strings.Split(id, "/")
		result := Result{InstanceID: id, RunID: p.RunID, State: p.State, Status: "not_run"}
		var failure error
		if blocked[id] {
			state, runID, e := r.graphState(id)
			if e != nil {
				result.State = ""
				result.StateUnknown = true
				out.Errors = append(out.Errors, GraphError{id, graphErrorCode(e), e.Error()})
				retErr = errors.Join(retErr, e)
			} else {
				result.State = state
				result.RunID = runID
			}
			out.Results = append(out.Results, result)
			continue
		}
		if err = ctx.Err(); err != nil {
			failure = &Error{"CANCELLED", err}
		} else {
			failure = r.recoverPreparation(ctx, parts[0], id)
			if failure == nil && p.RunID != "" {
				failure = r.stopLocked(ctx, parts[0], id, p.RunID)
			}
		}
		if failure != nil {
			block(id)
			result.Status = "failed"
			code := graphErrorCode(failure)
			out.Errors = append(out.Errors, GraphError{id, code, failure.Error()})
			retErr = errors.Join(retErr, failure)
		} else {
			result.Status = "unchanged"
			if p.RunID != "" && p.State != "stopped" {
				result.Status = "completed"
			}
			if e := r.pruneRuns(id); e != nil {
				e = &Error{"IO_ERROR", e}
				out.Errors = append(out.Errors, GraphError{id, graphErrorCode(e), e.Error()})
				retErr = errors.Join(retErr, e)
			}
		}
		observed, runID, readErr := r.graphState(id)
		if readErr != nil {
			result.State = ""
			result.StateUnknown = true
			block(id)
			out.Errors = append(out.Errors, GraphError{id, graphErrorCode(readErr), readErr.Error()})
			retErr = errors.Join(retErr, readErr)
		} else {

			result.State = observed
			result.RunID = runID
		}

		out.Results = append(out.Results, result)
	}
	return out, retErr
}

func (r *Runner) graphState(id string) (string, string, error) {
	parts := strings.Split(id, "/")
	if len(parts) != 3 {
		return "", "", errors.New("invalid instance id")
	}
	reg, e := r.Store.Read()
	if e != nil {
		return "", "", e
	}
	project, e := reg.Project(parts[0])
	if e != nil {
		return "", "", e
	}
	instance, ok := project.Instances[id]
	if !ok {
		return "", "", os.ErrNotExist
	}
	observed, e := r.Store.Inspect(instance)
	if observed == "stale" {
		if instance.State == "stopped" {
			observed = "stopped"
		} else {
			observed = "failed"
		}
	}
	return observed, instance.RunID, e
}
