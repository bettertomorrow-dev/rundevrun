package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

type GraphError struct {
	InstanceID string `json:"instance_id"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}
type GraphResult struct {
	Status  string       `json:"status"`
	Results []Result     `json:"results"`
	Errors  []GraphError `json:"errors"`
}
type upWork struct {
	plan               PlannedInstance
	project            *discovery.Project
	worktree           *discovery.Worktree
	runID, operationID string
	port               int
	pending            bool
	selected           bool
	prepared           *preparedRun
	oldPort            int
}

// UpGraph plans and starts each exact dependency before its consumers.
func (r *Runner) UpGraph(ctx context.Context, selections []Selection) (out GraphResult, retErr error) {
	return r.startGraph(ctx, "up", selections)
}
func (r *Runner) RestartGraph(ctx context.Context, selections []Selection) (GraphResult, error) {
	return r.startGraph(ctx, "restart", selections)
}
func (r *Runner) startGraph(ctx context.Context, action string, selections []Selection) (out GraphResult, retErr error) {
	out = GraphResult{Status: "ok", Results: []Result{}, Errors: []GraphError{}}
	defer func() {
		if retErr != nil {
			out.Status = "error"
			for _, result := range out.Results {
				if result.Status == "completed" || result.Status == "unchanged" {
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
	selected := map[string]bool{}
	for _, selection := range selections {
		id, e := selectionID(selection)
		if e != nil {
			return out, e
		}
		selected[id] = true
	}
	initial, err := r.PlanGraph(ctx, action, selections, false)
	if err != nil {
		return out, err
	}
	operationID, err := randomID()
	if err != nil {
		return out, err
	}
	ids := make([]string, len(initial.Instances))
	for i, p := range initial.Instances {
		ids[i] = p.InstanceID
	}
	locks, err := r.Store.LockInstances(ids...)
	if err != nil {
		if errors.Is(err, state.ErrBusy) {
			return out, &Error{"BUSY", err}
		}
		return out, err
	}
	defer locks.Close()
	for _, p := range initial.Instances {
		parts := strings.Split(p.InstanceID, "/")
		if err = r.recoverHook(ctx, parts[0], p.InstanceID, action, operationID); err != nil {
			return out, err
		}
		if err = r.recoverPreparation(ctx, parts[0], p.InstanceID); err != nil {
			return out, err
		}
	}
	plan, err := r.PlanGraph(ctx, action, selections, false)
	if err != nil {
		return out, err
	}
	if len(plan.Instances) != len(ids) {
		return out, &Error{"STATE_CONFLICT", errors.New("graph changed after locking")}
	}
	old := map[string]bool{}
	for _, id := range ids {
		old[id] = true
	}
	for _, p := range plan.Instances {
		if !old[p.InstanceID] {
			return out, &Error{"STATE_CONFLICT", errors.New("graph changed after locking")}
		}
	}
	// Wait for an already-authorized supervisor rather than starting a duplicate.
	for _, p := range plan.Instances {
		if p.State != "starting" {
			continue
		}
		wait, cancel := context.WithTimeout(ctx, 30*time.Second)
		for {
			reg, e := r.Store.Read()
			if e != nil {
				cancel()
				return out, e
			}
			parts := strings.Split(p.InstanceID, "/")
			project, e := reg.Project(parts[0])
			if e != nil {
				cancel()
				return out, e
			}
			inst := project.Instances[p.InstanceID]
			observed, e := r.Store.Inspect(inst)
			if e != nil {
				cancel()
				return out, e
			}
			if observed != "starting" {
				break
			}
			select {
			case <-wait.Done():
				cancel()
				if ctx.Err() != nil {
					return out, &Error{"CANCELLED", ctx.Err()}
				}
				return out, &Error{"BUSY", fmt.Errorf("instance %s is still starting", p.InstanceID)}
			case <-time.After(25 * time.Millisecond):
			}
		}
		cancel()
	}
	plan, err = r.PlanGraph(ctx, action, selections, false)
	if err != nil {
		return out, err
	}
	if len(plan.Instances) != len(old) {
		return out, &Error{"STATE_CONFLICT", errors.New("graph changed after waiting")}
	}
	for _, p := range plan.Instances {
		if !old[p.InstanceID] {
			return out, &Error{"STATE_CONFLICT", errors.New("graph changed after waiting")}
		}
	}
	reg, err := r.Store.Read()
	if err != nil {
		return out, err
	}
	work := make([]upWork, len(plan.Instances))
	projects := map[string]*discovery.Project{}
	for i, p := range plan.Instances {
		work[i].plan = p
		work[i].selected = action == "restart" && selected[p.InstanceID]
		if liveState(p.State) && !work[i].selected {
			continue
		}
		parts := strings.Split(p.InstanceID, "/")
		saved, e := reg.Project(parts[0])
		if e != nil {
			return out, e
		}
		inst := saved.Instances[p.InstanceID]
		if work[i].selected && liveState(p.State) {
			work[i].oldPort = inst.Reservations[inst.RunID]
		}
		var root string
		for _, w := range saved.Worktrees {
			if w.ID == parts[1] {
				root = w.Root
			}
		}
		fresh := projects[parts[0]]
		if fresh == nil {
			fresh, e = discovery.Discover(root)
			if e != nil {
				return out, e
			}
			projects[parts[0]] = fresh
		}
		if fresh.ID != parts[0] {
			return out, &Error{"STATE_CONFLICT", errors.New("project changed")}
		}
		w, e := discovery.Resolve(fresh, parts[1])
		if e != nil {
			return out, e
		}
		if w.Root != root || w.Config.Digest != p.Digest {
			return out, &Error{"STATE_CONFLICT", errors.New("configuration changed")}
		}
		if _, ok := w.Config.Services[parts[2]]; !ok {
			return out, &Error{"NOT_FOUND", errors.New("service missing")}
		}
		work[i].project = fresh
		work[i].worktree = w
		work[i].runID, e = randomID()
		if e != nil {
			return out, e
		}
		work[i].operationID = operationID
	}
	err = r.Store.Update(func(reg *state.Registry) error {
		if reg.Revision != plan.Revision {
			return &Error{"STATE_CONFLICT", errors.New("registry changed")}
		}
		used := map[int]bool{}
		for id := range reg.Projects {
			p, e := reg.Project(id)
			if e != nil {
				return e
			}
			for _, inst := range p.Instances {
				for _, v := range inst.Reservations {
					used[v] = true
				}
			}
		}
		for i := range work {
			x := &work[i]
			if x.project == nil {
				continue
			}
			svc := x.worktree.Config.Services[strings.Split(x.plan.InstanceID, "/")[2]]
			if fixed, ok := svc.Port.(int64); ok {
				if used[int(fixed)] && x.oldPort != int(fixed) {
					return &Error{"PORT_CONFLICT", errors.New("fixed port reserved")}
				}
				used[int(fixed)] = true
			}
		}
		for i := range work {
			x := &work[i]
			if x.project == nil {
				continue
			}
			svc := x.worktree.Config.Services[strings.Split(x.plan.InstanceID, "/")[2]]
			if svc.Port != nil {
				if x.oldPort != 0 && (svc.Port == "auto" || svc.Port == int64(x.oldPort)) {
					x.port = x.oldPort
				}
				start, end := 20000, 65535
				if fixed, ok := svc.Port.(int64); ok {
					start, end = int(fixed), int(fixed)
				}
				for port := start; x.port == 0 && port <= end; port++ {
					if svc.Port == "auto" && used[port] {
						continue
					}
					l, e := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
					if e != nil {
						continue
					}
					e = l.Close()
					if e != nil {
						return e
					}
					x.port = port
					used[port] = true
					break
				}
				if x.port == 0 {
					return &Error{"PORT_CONFLICT", errors.New("no available port")}
				}
			}
			p, e := reg.Project(x.project.ID)
			if e != nil {
				return e
			}
			inst := p.Instances[x.plan.InstanceID]
			if inst.Preparation != nil {
				return state.ErrBusy
			}
			if inst.Reservations == nil {
				inst.Reservations = map[string]int{}
			}
			if x.port != 0 {
				inst.Reservations[x.runID] = x.port
			}
			inst.Preparation = &state.Intent{RunID: x.runID, OperationID: x.operationID, Phase: "preparing"}
			p.Instances[inst.ID] = inst
			if e = reg.SetProject(p.ID, p); e != nil {
				return e
			}
			x.pending = true
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	defer func() {
		for i := range work {
			x := &work[i]
			if x.pending {
				cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				e := r.recoverPreparation(cleanup, x.project.ID, x.plan.InstanceID)
				cancel()
				retErr = errors.Join(retErr, e)
				if e != nil {
					out.Errors = append(out.Errors, GraphError{x.plan.InstanceID, graphErrorCode(e), e.Error()})
				}
			}
		}
		if retErr != nil {
			for i := range out.Results {
				item := &out.Results[i]
				observed, runID, e := r.graphState(item.InstanceID)
				if e != nil {
					item.State = ""
					item.StateUnknown = true
					out.Errors = append(out.Errors, GraphError{item.InstanceID, graphErrorCode(e), e.Error()})
					retErr = errors.Join(retErr, e)
				} else {
					item.State = observed
					item.RunID = runID
				}
			}
		}
	}()
	addresses := map[string]map[string]string{}
	for i := range work {
		x := &work[i]
		a := x.plan.Addresses
		if x.project != nil {
			a = map[string]string{"host": "127.0.0.1"}
			if x.port != 0 {
				a["port"] = strconv.Itoa(x.port)
				if x.worktree.Config.Services[strings.Split(x.plan.InstanceID, "/")[2]].Protocol == "http" {
					a["direct_url"] = "http://127.0.0.1:" + a["port"]
					a["url"] = a["direct_url"]
				}
			}
		}
		addresses[x.plan.InstanceID] = a
	}
	// Prepare every selected replacement before stopping any old run. New dependencies
	// with no old run can start immediately and satisfy downstream startup gates.
	for i := range work {
		x := &work[i]
		p := x.plan
		if x.project == nil {
			result, e := r.result(p.InstanceID, p.RunID, "unchanged")
			if e != nil {
				return out, e
			}
			out.Results = append(out.Results, result)
			continue
		}
		var e error
		for _, depID := range p.Bindings {
			if e = r.dependencyReady(depID); e != nil {
				break
			}
		}
		if e != nil {
			result, err := graphStartFailure(out, work, i, e)
			for j := range result.Results {
				if result.Results[j].InstanceID == p.InstanceID {
					result.Results[j].Status = "not_run"
				}
			}
			return result, err
		}
		if e == nil {
			deps := map[string]map[string]string{}
			for name, id := range p.Bindings {
				deps[name] = addresses[id]
			}
			service := strings.Split(p.InstanceID, "/")[2]
			legacy := Plan{InstanceID: p.InstanceID, State: p.State, RunID: p.RunID, Digest: p.Digest}
			var built preparedRun
			built, e = r.prepareLocked(ctx, x.project.ID, x.project, x.worktree, service, x.worktree.Config.Services[service], legacy, x.runID, x.operationID, x.port, deps, p.Bindings)
			if e == nil {
				built.reason = action
				x.prepared = &built
			}
		}
		if e == nil && !(x.selected && liveState(p.State)) {
			e = r.activateGraphNode(ctx, x, &out)
		}
		if e != nil {
			return graphStartFailure(out, work, i, e)
		}
	}
	for i := range work {
		x := &work[i]
		if x.prepared == nil || !x.pending {
			continue
		}
		if e := r.activateGraphNode(ctx, x, &out); e != nil {
			return graphStartFailure(out, work, i, e)
		}
	}

	return out, nil
}

func (r *Runner) dependencyReady(id string) error {
	parts := strings.Split(id, "/")
	if len(parts) != 3 {
		return &Error{"STATE_CORRUPT", errors.New("invalid dependency id")}
	}
	reg, e := r.Store.Read()
	if e != nil {
		return e
	}
	project, e := reg.Project(parts[0])
	if e != nil {
		return e
	}
	instance, ok := project.Instances[id]
	if !ok {
		return os.ErrNotExist
	}
	if instance.Hook != nil && instance.Hook.RunID == instance.RunID && instance.Hook.Name == "after_start" && instance.Hook.Phase != "completed" {
		return &Error{"HOOK_FAILED", fmt.Errorf("dependency %s has failed after_start hook", id)}
	}
	state, _, err := r.graphState(id)
	if err != nil {
		return &Error{"OWNERSHIP_UNKNOWN", err}
	}
	if state != "running" && state != "ready" {
		return &Error{"READINESS_TIMEOUT", fmt.Errorf("dependency %s is %s", id, state)}
	}
	return nil
}
func (r *Runner) activateGraphNode(ctx context.Context, x *upWork, out *GraphResult) error {
	for _, id := range x.plan.Bindings {
		if e := r.dependencyReady(id); e != nil {
			return e
		}
	}
	result, e := r.activateLocked(ctx, x.prepared)
	if e == nil {
		x.pending = false
		out.Results = append(out.Results, result)
		if err := r.pruneRuns(x.plan.InstanceID); err != nil {
			return &Error{"IO_ERROR", err}
		}
		return nil
	}
	if x.prepared.active {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := r.stopLocked(cleanup, x.project.ID, x.plan.InstanceID, x.runID)
		cancel()
		x.pending = false
		if err != nil {
			out.Errors = append(out.Errors, GraphError{x.plan.InstanceID, graphErrorCode(err), err.Error()})
		}
		e = errors.Join(e, err)
	}
	return e
}
func graphStartFailure(out GraphResult, work []upWork, index int, e error) (GraphResult, error) {
	p := work[index].plan
	code := graphErrorCode(e)
	out.Errors = append(out.Errors, GraphError{p.InstanceID, code, e.Error()})
	seen := map[string]bool{}
	for _, v := range out.Results {
		seen[v.InstanceID] = true
	}
	if !seen[p.InstanceID] {
		out.Results = append(out.Results, Result{InstanceID: p.InstanceID, State: p.State, RunID: p.RunID, Status: "failed"})
		seen[p.InstanceID] = true
	}
	for _, x := range work {
		if !seen[x.plan.InstanceID] {
			out.Results = append(out.Results, Result{InstanceID: x.plan.InstanceID, State: x.plan.State, RunID: x.plan.RunID, Status: "not_run"})
		}
	}
	return out, e
}

func graphErrorCode(err error) string {
	var runnerErr *Error
	var configErr *config.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "CANCELLED"
	case errors.As(err, &runnerErr):
		return runnerErr.Code
	case errors.Is(err, context.DeadlineExceeded):
		return "CANCELLED"
	case errors.Is(err, state.ErrCorrupt):
		return "STATE_CORRUPT"
	case errors.Is(err, state.ErrBusy):
		return "BUSY"
	case errors.Is(err, discovery.ErrAmbiguous):
		return "AMBIGUOUS"
	case errors.Is(err, discovery.ErrNotFound), errors.Is(err, os.ErrNotExist):
		return "NOT_FOUND"
	case errors.Is(err, config.ErrVersionUnsupported):
		return "VERSION_UNSUPPORTED"
	case errors.As(err, &configErr):
		return configErr.Code
	default:
		return "IO_ERROR"
	}
}
