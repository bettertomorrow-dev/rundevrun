// Executes service worktree switches and recovers interrupted switches.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

type SwitchStepResult = state.OperationStep
type SwitchResult = GraphResult
type switchWork struct {
	plan     PlannedInstance
	project  *discovery.Project
	worktree *discovery.Worktree
	runID    string
	port     int
	oldPort  int
	prepared *preparedRun
	pending  bool
	borrowed bool
}

func (r *Runner) SwitchGraph(ctx context.Context, mappings []SwitchMapping) (out SwitchResult, retErr error) {
	return r.switchGraph(ctx, mappings, nil)
}

func (r *Runner) switchGraph(ctx context.Context, mappings []SwitchMapping, starts []Selection) (out SwitchResult, retErr error) {
	out = GraphResult{Status: "ok", Results: []Result{}, Errors: []GraphError{}}
	out.Steps = []SwitchStepResult{}
	defer func() {
		if retErr != nil {
			out.Status = "error"
			for _, v := range out.Results {
				if v.Status == "completed" {
					out.Status = "partial"
					break
				}
			}
			for _, step := range out.Steps {
				if step.Status == "completed" && (strings.HasPrefix(step.ID, "stop:") || strings.HasPrefix(step.ID, "start:")) {
					out.Status = "partial"
					break
				}
			}
			if len(out.Errors) == 0 {
				out.Errors = append(out.Errors, GraphError{Code: graphErrorCode(retErr), Message: retErr.Error()})
			}
		}
	}()
	initial, e := r.planSwitch(ctx, mappings, starts, nil)
	if e != nil {
		return out, e
	}
	serviceLocks, e := r.lockServiceStarts(initial.Graph)
	if e != nil {
		return out, e
	}
	defer serviceLocks.Close()
	opID, e := randomID()
	if e != nil {
		return out, e
	}
	locks, e := r.Store.LockInstances(initial.Locks...)
	if e != nil {
		if errors.Is(e, state.ErrBusy) {
			return out, &Error{"BUSY", e}
		}
		return out, e
	}
	defer locks.Close()
	lockedPlan, e := r.planSwitch(ctx, mappings, starts, nil)
	if e != nil {
		return out, e
	}
	if initial.Revision != lockedPlan.Revision || !reflect.DeepEqual(initial.Locks, lockedPlan.Locks) || !reflect.DeepEqual(initial.Digests, lockedPlan.Digests) || !reflect.DeepEqual(initial.Steps, lockedPlan.Steps) {
		return out, &Error{"STATE_CONFLICT", errors.New("switch graph changed after locking")}
	}
	initial = lockedPlan
	oldIDs := map[string]bool{}
	for _, node := range initial.OldStops {
		oldIDs[node.InstanceID] = true
	}
	for _, node := range initial.NewStarts {
		if !oldIDs[node.InstanceID] && node.RunID != "" && node.State != "stopped" {
			return out, &Error{"OWNERSHIP_UNKNOWN", fmt.Errorf("target %s requires explicit down/recovery", node.InstanceID)}
		}
	}
	raw, e := json.Marshal(initial)
	if e != nil {
		return out, e
	}
	owner, e := procid.Observe(os.Getpid())
	if e != nil {
		return out, e
	}
	journal := state.Operation{ID: opID, Kind: "switch", Status: "running", Owner: owner, Plan: raw, Runs: map[string]string{}, Steps: []state.OperationStep{}}
	for _, node := range initial.OldStops {
		if node.RunID != "" {
			journal.Runs["old:"+node.InstanceID] = node.RunID
		}
	}
	for _, step := range initial.Steps {
		journal.Steps = append(journal.Steps, state.OperationStep{ID: step.ID, Status: "not_run"})
	}
	out.OperationID = opID
	out.JournalPath, e = r.Store.OperationPath(opID)
	if e != nil {
		return out, e
	}
	if e = r.Store.WriteOperation(journal); e != nil {
		return out, e
	}
	reportPlan := initial
	defer func() {
		out.Steps = append([]SwitchStepResult(nil), journal.Steps...)
		resultIDs := map[string]bool{}
		for _, p := range reportPlan.OldStops {
			resultIDs[p.InstanceID] = true
		}
		for _, p := range reportPlan.NewStarts {
			resultIDs[p.InstanceID] = true
		}
		for _, rewrite := range reportPlan.BindingRewrites {
			resultIDs[rewrite.ConsumerID] = true
		}
		ids := make([]string, 0, len(resultIDs))
		for id := range resultIDs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		statuses := map[string]string{}
		for _, step := range journal.Steps {
			statuses[step.ID] = step.Status
		}
		for _, id := range ids {
			result := Result{InstanceID: id, Status: "not_run"}
			lifecycle := false
			for _, action := range []string{"prepare:", "stop:", "start:"} {
				if status, ok := statuses[action+id]; ok && result.Status != "failed" {
					lifecycle = true
					result.Status = status
					if status == "started" {
						result.Status = "failed"
					}
				}
			}
			bindingOnlyComplete := !lifecycle
			for _, rewrite := range reportPlan.BindingRewrites {
				if rewrite.ConsumerID != id {
					continue
				}
				status := statuses["bind:"+id+":"+rewrite.Dependency]
				if status == "failed" || status == "started" {
					result.Status = "failed"
				}
				bindingOnlyComplete = bindingOnlyComplete && status == "completed"
			}
			if bindingOnlyComplete {
				result.Status = "completed"
			}
			observed, run, err := r.graphState(id)
			if err != nil {
				result.StateUnknown = true
				out.Errors = append(out.Errors, GraphError{id, graphErrorCode(err), err.Error()})
				retErr = errors.Join(retErr, err)
			} else {
				result.State = observed
				result.RunID = run
			}
			out.Results = append(out.Results, result)
		}
	}()
	defer func() {
		if retErr != nil && journal.Status == "running" {
			journal.Status = "failed"
			retErr = errors.Join(retErr, r.Store.WriteOperation(journal))
		}
		out.Steps = append([]SwitchStepResult(nil), journal.Steps...)
	}()
	targetBySource := map[string]string{}
	for _, pair := range initial.Mappings {
		targetBySource[pair.From] = pair.To
	}
	rootsByID := map[string]string{}
	for _, node := range initial.NewStarts {
		if node.routeSelection.Worktree != nil {
			rootsByID[node.InstanceID] = node.routeSelection.Worktree.Root
		}
	}
	affected := map[string]bool{}
	for _, p := range initial.OldStops {
		affected[p.InstanceID] = true
	}
	for _, p := range initial.NewStarts {
		affected[p.InstanceID] = true
	}

	for _, id := range initial.Locks {
		parts := strings.Split(id, "/")
		if len(parts) != 3 {
			return out, &Error{"STATE_CORRUPT", errors.New("invalid graph id")}
		}
		snapshot, err := r.Store.Read()
		if err != nil {
			return out, err
		}
		project, err := snapshot.Project(parts[0])
		if err != nil {
			return out, err
		}
		previous := project.Instances[id].Hook
		recoveryIndex := -1
		if previous != nil && previous.Phase != "completed" {
			journal.Runs["hook:"+id] = previous.RunID
			recoveryIndex = len(journal.Steps)
			journal.Steps = append(journal.Steps, state.OperationStep{ID: "recover_hook:" + id, Status: "started"})
			if err = r.Store.WriteOperation(journal); err != nil {
				return out, err
			}
		}
		recoveryRunner := r
		if !affected[id] && r.RetryHooks {
			copyRunner := *r
			copyRunner.RetryHooks = false
			recoveryRunner = &copyRunner
		}
		newWorktree := rootsByID[id]
		if target := targetBySource[id]; target != "" {
			newWorktree = rootsByID[target]
		}
		e = recoveryRunner.recoverHook(ctx, parts[0], id, "switch", newWorktree, opID)
		if recoveryIndex >= 0 {
			status := "completed"
			code := ""
			if e != nil {
				status = "failed"
				code = graphErrorCode(e)
			}
			journal.Steps[recoveryIndex].Status = status
			journal.Steps[recoveryIndex].Code = code
			if err = r.Store.WriteOperation(journal); err != nil {
				return out, errors.Join(e, err)
			}
		}
		if e != nil {
			return out, e
		}
		if e = r.recoverPreparation(ctx, parts[0], id); e != nil {
			return out, e
		}

	}
	plan, e := r.planSwitch(ctx, mappings, starts, nil)
	if e != nil {
		return out, e
	}
	reportPlan = plan
	if !reflect.DeepEqual(initial.Locks, plan.Locks) || !reflect.DeepEqual(initial.Digests, plan.Digests) || !reflect.DeepEqual(initial.Steps, plan.Steps) {
		return out, &Error{"STATE_CONFLICT", errors.New("switch graph changed after locking")}
	}
	reg, e := r.Store.Read()
	if e != nil {
		return out, e
	}
	if reg.Revision != plan.Revision {
		return out, &Error{"STATE_CONFLICT", errors.New("registry changed")}
	}
	work := map[string]*switchWork{}
	projects := map[string]*discovery.Project{}
	for _, node := range plan.NewStarts {
		parts := strings.Split(node.InstanceID, "/")
		saved, err := reg.Project(parts[0])
		if err != nil {
			return out, err
		}
		var root string
		for _, w := range saved.Worktrees {
			if w.ID == parts[1] {
				root = w.Root
				break
			}
		}
		project := projects[parts[0]]
		if project == nil {
			project, err = discovery.Discover(root)
			if err != nil {
				return out, err
			}
			if project.ID != parts[0] {
				return out, &Error{"STATE_CONFLICT", errors.New("project identity changed")}
			}
			projects[parts[0]] = project
		}
		w, err := discovery.Resolve(project, parts[1])
		if err != nil {
			return out, err
		}
		if w.Root != root || w.Config.Digest != plan.Digests[node.InstanceID] {
			return out, &Error{"STATE_CONFLICT", errors.New("configuration changed")}
		}
		if _, ok := w.Config.Services[parts[2]]; !ok {
			return out, &Error{"NOT_FOUND", errors.New("service missing")}
		}
		runID, err := randomID()
		if err != nil {
			return out, err
		}
		inst := saved.Instances[node.InstanceID]
		work[node.InstanceID] = &switchWork{plan: node, project: project, worktree: w, runID: runID, oldPort: inst.Reservations[inst.RunID]}
	}
	for id, digest := range plan.Digests {
		parts := strings.Split(id, "/")
		if len(parts) != 3 {
			return out, &Error{"STATE_CORRUPT", errors.New("invalid digest id")}
		}
		p := projects[parts[0]]
		if p == nil {
			return out, &Error{"STATE_CONFLICT", errors.New("missing planned project")}
		}
		w, err := discovery.Resolve(p, parts[1])
		if err != nil {
			return out, err
		}
		if w.Config.Digest != digest {
			return out, &Error{"STATE_CONFLICT", fmt.Errorf("configuration changed for %s", id)}
		}
	}
	raw, e = json.Marshal(plan)
	if e != nil {
		return out, e
	}
	journal.Plan = raw
	for id, x := range work {
		journal.Runs[id] = x.runID
	}
	if e = r.Store.WriteOperation(journal); e != nil {
		return out, e
	}

	sourceFor := map[string]string{}
	for _, pair := range plan.Mappings {
		sourceFor[pair.To] = pair.From
	}
	e = r.Store.Update(func(current *state.Registry) error {
		if current.Revision != plan.Revision {
			return &Error{"STATE_CONFLICT", errors.New("registry changed")}
		}
		if err := r.ReservePlannedRoutes(current, GraphPlan{Action: "switch", Revision: plan.Revision, Instances: plan.Graph}); err != nil {
			return err
		}
		used := map[int]string{}
		for pid := range current.Projects {
			p, err := current.Project(pid)
			if err != nil {
				return err
			}
			for id, i := range p.Instances {
				for _, port := range i.Reservations {
					if prior := used[port]; prior != "" && prior != id {
						return &Error{"STATE_CORRUPT", fmt.Errorf("conflicting port reservations for %s and %s", prior, id)}
					}
					used[port] = id
				}
			}
		}
		for _, node := range plan.NewStarts {
			x := work[node.InstanceID]
			svc := x.worktree.Config.Services[strings.Split(node.InstanceID, "/")[2]]
			if fixed, ok := svc.Port.(int64); ok {
				port := int(fixed)
				if owner := used[port]; owner != "" && !(owner == node.InstanceID && x.oldPort == port) {
					if owner != sourceFor[node.InstanceID] {
						return &Error{"PORT_CONFLICT", errors.New("fixed port reserved")}
					}
					p, err := current.Project(x.project.ID)
					if err != nil {
						return err
					}
					source := p.Instances[owner]
					for runID, held := range source.Reservations {
						if held == port && runID != source.RunID {
							return &Error{"PORT_CONFLICT", errors.New("source has an additional port reservation")}
						}
					}
					if source.Reservations[source.RunID] != port {
						return &Error{"PORT_CONFLICT", errors.New("source does not own fixed port")}
					}
					x.port, x.borrowed = port, true
				}
				used[port] = node.InstanceID
			}
		}
		for _, node := range plan.NewStarts {
			x := work[node.InstanceID]
			svc := x.worktree.Config.Services[strings.Split(node.InstanceID, "/")[2]]
			if svc.Port != nil {
				if x.oldPort != 0 && (svc.Port == "auto" || svc.Port == int64(x.oldPort)) {
					x.port = x.oldPort
				}
				start, end := 20000, 65535
				if fixed, ok := svc.Port.(int64); ok {
					start, end = int(fixed), int(fixed)
				}
				for port := start; x.port == 0 && port <= end; port++ {
					if svc.Port == "auto" && used[port] != "" {
						continue
					}
					l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
					if err != nil {
						continue
					}
					err = l.Close()
					if err != nil {
						return err
					}
					x.port = port
					used[port] = node.InstanceID
				}
				if x.port == 0 {
					return &Error{"PORT_CONFLICT", errors.New("no available port")}
				}
			}
			p, err := current.Project(x.project.ID)
			if err != nil {
				return err
			}
			inst, ok := p.Instances[node.InstanceID]
			if !ok {
				return os.ErrNotExist
			}
			if inst.Preparation != nil {
				return state.ErrBusy
			}
			if inst.Reservations == nil {
				inst.Reservations = map[string]int{}
			}
			if x.port != 0 && !x.borrowed {
				inst.Reservations[x.runID] = x.port
			}
			inst.Preparation = &state.Intent{RunID: x.runID, OperationID: opID, Phase: "preparing"}
			p.Instances[inst.ID] = inst
			if err = current.SetProject(p.ID, p); err != nil {
				return err
			}
			x.pending = true
		}
		return nil
	})
	if e != nil {
		journal.Status = "failed"
		_ = r.Store.WriteOperation(journal)
		return out, e
	}
	defer func() {
		for _, x := range work {
			if !x.pending {
				continue
			}
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := r.recoverPreparation(cleanup, x.project.ID, x.plan.InstanceID)
			cancel()
			if err != nil {
				out.Errors = append(out.Errors, GraphError{x.plan.InstanceID, graphErrorCode(err), err.Error()})
				retErr = errors.Join(retErr, err)
			}
		}
		journal.Status = "completed"
		if retErr != nil {
			journal.Status = "failed"
		}
		journalErr := r.Store.WriteOperation(journal)
		if journalErr != nil {
			retErr = errors.Join(retErr, journalErr)
		}
		if retErr == nil && journalErr == nil {
			ids := map[string]bool{}
			for _, node := range plan.OldStops {
				ids[node.InstanceID] = true
			}
			for _, node := range plan.NewStarts {
				ids[node.InstanceID] = true
			}
			ordered := make([]string, 0, len(ids))
			for id := range ids {
				ordered = append(ordered, id)
			}
			sort.Strings(ordered)
			for _, id := range ordered {
				if e := r.pruneRuns(id); e != nil {
					wrapped := &Error{"IO_ERROR", e}
					out.Errors = append(out.Errors, GraphError{id, "IO_ERROR", e.Error()})
					retErr = errors.Join(retErr, wrapped)
				}
			}
		}
	}()
	addresses := map[string]map[string]string{}
	for _, node := range plan.Graph {
		addresses[node.InstanceID] = node.Addresses
	}
	for _, node := range plan.NewStarts {
		x := work[node.InstanceID]
		addr := map[string]string{"host": "127.0.0.1"}
		if x.port != 0 {
			addr["port"] = strconv.Itoa(x.port)
			svc := x.worktree.Config.Services[strings.Split(node.InstanceID, "/")[2]]
			if svc.Protocol == "http" {
				addr["direct_url"] = "http://127.0.0.1:" + addr["port"]
				addr["url"] = addr["direct_url"]
				if node.route.url != "" {
					addr["url"] = node.route.url
				}
			}
		}
		addresses[node.InstanceID] = addr
	}
	old := map[string]PlannedInstance{}
	for _, p := range plan.OldStops {
		old[p.InstanceID] = p
		if _, ok := work[p.InstanceID]; ok {
			sourceFor[p.InstanceID] = p.InstanceID
		}
	}
	stepIndex := map[string]int{}
	byID := map[string]SwitchStep{}
	for i, s := range plan.Steps {
		stepIndex[s.ID] = i
		byID[s.ID] = s
	}
	dead := map[string]bool{}
	pending := map[string]bool{}
	for _, s := range plan.Steps {
		pending[s.ID] = true
	}
	for len(pending) > 0 {
		progress := false
		for _, step := range plan.Steps {
			if !pending[step.ID] {
				continue
			}
			ready := true
			blocked := false
			for _, dep := range step.DependsOn {
				idx, ok := stepIndex[dep]
				if !ok {
					return out, &Error{"STATE_CORRUPT", fmt.Errorf("missing step %s", dep)}
				}
				st := journal.Steps[idx].Status
				if st == "not_run" && pending[dep] {
					ready = false
					break
				}
				if st != "completed" {
					if step.Action == "stop" && byID[dep].Action == "stop" && dead[byID[dep].InstanceID] {
						continue
					}
					blocked = true
				}
			}
			if !ready {
				continue
			}
			progress = true
			delete(pending, step.ID)
			idx := stepIndex[step.ID]
			if blocked || ctx.Err() != nil {
				journal.Steps[idx].Status = "not_run"
				if ctx.Err() != nil {
					retErr = errors.Join(retErr, ctx.Err())
				}
				if err := r.Store.WriteOperation(journal); err != nil {
					return out, errors.Join(retErr, err)
				}
				continue
			}
			journal.Steps[idx].Status = "started"
			if err := r.Store.WriteOperation(journal); err != nil {
				return out, errors.Join(retErr, err)
			}
			err := r.switchStep(ctx, step, opID, work, old, sourceFor, addresses, plan.BindingRewrites, dead, &out)
			if err != nil {
				journal.Steps[idx].Status = "failed"
				journal.Steps[idx].Code = graphErrorCode(err)
				out.Errors = append(out.Errors, GraphError{step.InstanceID, graphErrorCode(err), err.Error()})
				retErr = errors.Join(retErr, err)
			} else {
				journal.Steps[idx].Status = "completed"
			}
			if e = r.Store.WriteOperation(journal); e != nil {
				return out, errors.Join(retErr, e)
			}
		}
		if !progress {
			return out, errors.Join(retErr, &Error{"CYCLE", errors.New("switch step dependency cycle")})
		}
	}

	return out, retErr
}

func (r *Runner) switchStep(ctx context.Context, step SwitchStep, operationID string, work map[string]*switchWork, old map[string]PlannedInstance, sourceFor map[string]string, addresses map[string]map[string]string, rewrites []BindingRewrite, dead map[string]bool, out *GraphResult) error {
	id := step.InstanceID
	parts := strings.Split(id, "/")
	if len(parts) != 3 {
		return &Error{"STATE_CORRUPT", errors.New("invalid instance id")}
	}
	switch step.Action {
	case "prepare":
		x := work[id]
		if x == nil {
			return &Error{"STATE_CORRUPT", errors.New("missing preparation")}
		}
		dependencies := map[string]map[string]string{}
		for name, target := range x.plan.Bindings {
			dependencies[name] = addresses[target]
			if sourceFor[target] != "" {
				continue
			}
			if e := r.dependencyReady(target); e != nil {
				return e
			}
		}
		svc := x.worktree.Config.Services[parts[2]]
		legacy := Plan{InstanceID: id, State: x.plan.State, RunID: x.plan.RunID, Digest: x.plan.Digest, Addresses: addresses[id]}
		prepared, e := r.prepareLocked(ctx, x.project.ID, x.project, x.worktree, parts[2], svc, legacy, x.runID, operationID, x.port, dependencies, x.plan.Bindings)
		if e != nil {
			return e
		}
		prepared.reason = "switch"
		prepared.newWorktree = x.worktree.Root
		if source := sourceFor[id]; source != "" {
			if prior, ok := old[source]; ok && prior.RunID != "" {
				m, err := r.Store.ReadManifest(prior.RunID)
				if err != nil {
					return err
				}
				prepared.oldWorktree = m.StopContract.Worktree
				if prepared.oldWorktree == "" {
					sourceParts := strings.Split(source, "/")
					if len(sourceParts) == 3 {
						for _, w := range x.project.Worktrees {
							if w.ID == sourceParts[1] {
								prepared.oldWorktree = w.Root
								break
							}
						}
					}
				}
			}
		}
		x.prepared = &prepared
		return nil
	case "stop":
		prior, ok := old[id]
		if !ok {
			return &Error{"STATE_CORRUPT", errors.New("missing stop record")}
		}
		newWorktree := ""
		for target, source := range sourceFor {
			if source == id {
				if x := work[target]; x != nil {
					newWorktree = x.worktree.Root
				}
				break
			}
		}
		if prior.RunID == "" {
			dead[id] = true
			return nil
		}
		err := r.stopWithHooks(ctx, parts[0], id, prior.RunID, "switch", newWorktree, operationID)
		observed, _, inspectErr := r.graphState(id)
		if inspectErr != nil {
			return errors.Join(err, inspectErr)
		}
		if observed == "stopped" {
			dead[id] = true
		}
		return err
	case "bind":
		var binding *BindingRewrite
		for i := range rewrites {
			v := &rewrites[i]
			if "bind:"+v.ConsumerID+":"+v.Dependency == step.ID {
				binding = v
				break
			}
		}
		if binding == nil {
			return &Error{"STATE_CORRUPT", errors.New("missing binding rewrite")}
		}
		if e := r.dependencyReady(binding.To); e != nil {
			return e
		}
		return r.Store.Update(func(reg *state.Registry) error {
			p, e := reg.Project(parts[0])
			if e != nil {
				return e
			}
			inst, ok := p.Instances[id]
			if !ok {
				return os.ErrNotExist
			}
			if inst.DesiredBindings[binding.Dependency] != binding.From {
				return &Error{"STATE_CONFLICT", errors.New("desired binding changed")}
			}
			if inst.DesiredBindings == nil {
				inst.DesiredBindings = map[string]string{}
			}
			inst.DesiredBindings[binding.Dependency] = binding.To
			p.Instances[id] = inst
			return reg.SetProject(p.ID, p)
		})
	case "start":
		x := work[id]
		if x == nil || x.prepared == nil {
			return &Error{"STATE_CORRUPT", errors.New("missing prepared run")}
		}
		for _, target := range x.plan.Bindings {
			if e := r.dependencyReady(target); e != nil {
				return e
			}
		}
		if x.borrowed {
			if err := r.reserveHandoffPort(x); err != nil {
				return err
			}
		}
		x.prepared.plan.State = "stopped"
		result, e := r.activateLocked(ctx, x.prepared)
		if e == nil {
			x.pending = false
			_ = result
			return nil
		}
		if x.prepared.active {
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			cleanupErr := r.stopLocked(cleanup, x.project.ID, id, x.runID)
			cancel()
			x.pending = false
			return errors.Join(e, cleanupErr)
		}
		if !x.prepared.reserved {
			x.pending = false
		}
		return e
	default:
		return &Error{"STATE_CORRUPT", fmt.Errorf("unknown switch step %s", step.Action)}
	}
}

func (r *Runner) reserveHandoffPort(x *switchWork) error {
	return r.Store.Update(func(reg *state.Registry) error {
		for pid := range reg.Projects {
			p, err := reg.Project(pid)
			if err != nil {
				return err
			}
			for _, inst := range p.Instances {
				for _, port := range inst.Reservations {
					if port == x.port {
						return &Error{"PORT_CONFLICT", errors.New("handoff port is still reserved")}
					}
				}
			}
		}
		p, err := reg.Project(x.project.ID)
		if err != nil {
			return err
		}
		inst := p.Instances[x.plan.InstanceID]
		if inst.Preparation == nil || inst.Preparation.RunID != x.runID {
			return &Error{"STATE_CONFLICT", errors.New("handoff preparation changed")}
		}
		if inst.Reservations == nil {
			inst.Reservations = map[string]int{}
		}
		inst.Reservations[x.runID] = x.port
		p.Instances[inst.ID] = inst
		return reg.SetProject(p.ID, p)
	})
}
