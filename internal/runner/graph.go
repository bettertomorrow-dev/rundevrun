package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

type Selection struct {
	Project  *discovery.Project
	Worktree *discovery.Worktree
	Service  string
}
type PlannedInstance struct {
	InstanceID string            `json:"instance_id"`
	State      string            `json:"state"`
	RunID      string            `json:"run_id,omitempty"`
	Digest     string            `json:"digest,omitempty"`
	Addresses  map[string]string `json:"addresses,omitempty"`
	Bindings   map[string]string `json:"bindings,omitempty"`
	Steps      []string          `json:"steps"`
	env        map[string]string
}
type GraphPlan struct {
	Action    string            `json:"action"`
	Revision  uint64            `json:"revision"`
	Instances []PlannedInstance `json:"instances"`
}
type BindResult struct {
	ConsumerID      string `json:"consumer_id"`
	TargetID        string `json:"target_id"`
	RestartRequired bool   `json:"restart_required"`
}

type graphNode struct {
	selection Selection
	saved     state.Instance
	service   config.Service
	plan      PlannedInstance
	live      bool
}
type graphPlanner struct {
	r        *Runner
	reg      state.Registry
	nodes    map[string]*graphNode
	projects map[string]*discovery.Project
	used     map[int]bool
	order    []string
	marks    map[string]int
	override map[string]map[string]string
	desired  bool
	replace  map[string]bool
}

func liveState(s string) bool {
	return s == "starting" || s == "running" || s == "ready" || s == "unhealthy"
}
func selectionID(s Selection) (string, error) {
	if s.Project == nil || s.Worktree == nil || s.Service == "" {
		return "", &Error{"USAGE", errors.New("missing selection")}
	}
	return s.Project.ID + "/" + s.Worktree.ID + "/" + s.Service, nil
}
func (g *graphPlanner) project(s Selection) (*discovery.Project, error) {
	if p := g.projects[s.Project.ID]; p != nil {
		return p, nil
	}
	p, e := discovery.Discover(s.Worktree.Root)
	if e != nil {
		return nil, e
	}
	if p.ID != s.Project.ID {
		return nil, &Error{"STATE_CONFLICT", errors.New("project identity changed")}
	}
	g.projects[p.ID] = p
	return p, nil
}
func (g *graphPlanner) load(s Selection, configured bool) (*graphNode, error) {
	id, e := selectionID(s)
	if e != nil {
		return nil, e
	}
	if n := g.nodes[id]; n != nil {
		return n, nil
	}
	p, e := g.reg.Project(s.Project.ID)
	if e != nil {
		return nil, e
	}
	saved, ok := p.Instances[id]
	if !ok {
		return nil, &Error{"NOT_FOUND", fmt.Errorf("instance %s not registered", id)}
	}
	known := false
	for _, w := range p.Worktrees {
		if w.ID == s.Worktree.ID && w.Root == s.Worktree.Root {
			known = true
		}
	}
	if !known {
		return nil, &Error{"NOT_FOUND", errors.New("worktree not registered")}
	}
	observed, e := g.r.Store.Inspect(saved)
	if e != nil {
		return nil, e
	}
	if observed == "stale" {
		observed = "failed"
	}
	n := &graphNode{selection: s, saved: saved, live: liveState(observed), plan: PlannedInstance{InstanceID: id, State: observed, RunID: saved.RunID, Steps: []string{}}}
	if saved.Preparation != nil {
		n.plan.Steps = append(n.plan.Steps, "recover_preparation")
	}
	if observed == "starting" {
		n.plan.Steps = append(n.plan.Steps, "recover_start", "readiness")
	}
	if n.live {
		m, err := g.r.Store.ReadManifest(saved.RunID)
		if err != nil {
			return nil, err
		}
		if m.InstanceID != id {
			return nil, fmt.Errorf("%w: manifest instance mismatch", state.ErrCorrupt)
		}
		if len(m.StopContract.Hooks) > 0 {
			return nil, &Error{"CONFIG_INVALID", errors.New("stop hooks are not supported by this stage")}
		}
		n.plan.Digest = m.Digest
		n.plan.Addresses = m.Addresses
		n.plan.Bindings = m.EffectiveBindings
	}
	if configured && (!n.live || g.desired || g.replace[id]) {
		project, err := g.project(s)
		if err != nil {
			return nil, err
		}
		w, err := discovery.Resolve(project, s.Worktree.ID)
		if err != nil {
			return nil, err
		}
		if w.Root != s.Worktree.Root {
			return nil, &Error{"STATE_CONFLICT", errors.New("worktree identity changed")}
		}
		svc, ok := w.Config.Services[s.Service]
		if !ok {
			return nil, &Error{"NOT_FOUND", fmt.Errorf("service %q not found", s.Service)}
		}
		n.selection.Project = project
		n.selection.Worktree = w
		if len(svc.Hooks) != 0 {
			return nil, &Error{"CONFIG_INVALID", errors.New("hooks are not supported by this stage")}
		}
		n.service = svc
		if !n.live || g.replace[id] {
			n.plan.Digest = w.Config.Digest
		}
	}
	g.nodes[id] = n
	return n, nil
}
func (g *graphPlanner) target(s Selection, id, dep string) (Selection, error) {
	parts := strings.Split(id, "/")
	if len(parts) != 3 || parts[0] != s.Project.ID || parts[2] != dep {
		return Selection{}, &Error{"BINDING_INVALID", fmt.Errorf("invalid target %q for %s", id, dep)}
	}
	p, e := g.reg.Project(s.Project.ID)
	if e != nil {
		return Selection{}, e
	}
	for _, w := range p.Worktrees {
		if w.ID == parts[1] {
			return Selection{s.Project, &discovery.Worktree{ID: w.ID, Root: w.Root}, dep}, nil
		}
	}
	return Selection{}, &Error{"BINDING_INVALID", fmt.Errorf("unknown target worktree %s", parts[1])}
}
func (g *graphPlanner) walk(s Selection) error {
	id, e := selectionID(s)
	if e != nil {
		return e
	}
	if g.marks[id] == 1 {
		return &Error{"CYCLE", fmt.Errorf("dependency cycle at %s", id)}
	}
	if g.marks[id] == 2 {
		return nil
	}
	g.marks[id] = 1
	n, e := g.load(s, true)
	if e != nil {
		return e
	}
	bindings := map[string]string{}
	if n.live && !g.desired && !g.replace[id] {
		for k, v := range n.plan.Bindings {
			bindings[k] = v
		}
	} else {
		for _, dep := range n.service.DependsOn {
			target := n.saved.DesiredBindings[dep]
			if x := g.override[id]; x != nil && x[dep] != "" {
				target = x[dep]
			}
			if target == "" {
				target = s.Project.ID + "/" + s.Worktree.ID + "/" + dep
			}
			bindings[dep] = target
		}
	}
	deps := make([]string, 0, len(bindings))
	for dep := range bindings {
		deps = append(deps, dep)
	}
	sort.Strings(deps)
	for _, dep := range deps {
		t, err := g.target(s, bindings[dep], dep)
		if err != nil {
			return err
		}
		if err = g.walk(t); err != nil {
			return err
		}
	}
	n.plan.Bindings = bindings
	g.marks[id] = 2
	g.order = append(g.order, id)
	return nil
}
func newGraph(r *Runner, reg state.Registry) *graphPlanner {
	return &graphPlanner{r: r, reg: reg, nodes: map[string]*graphNode{}, projects: map[string]*discovery.Project{}, used: map[int]bool{}, marks: map[string]int{}}
}
func (r *Runner) PlanGraph(ctx context.Context, action string, selections []Selection, cascade bool) (GraphPlan, error) {
	if err := ctx.Err(); err != nil {
		return GraphPlan{}, &Error{"CANCELLED", err}
	}
	if r == nil || r.Store == nil || len(selections) == 0 || (action != "up" && action != "down" && action != "restart") {
		return GraphPlan{}, &Error{"USAGE", errors.New("invalid graph request")}
	}
	reg, e := r.Store.Read()
	if e != nil {
		return GraphPlan{}, e
	}
	g := newGraph(r, reg)
	result := GraphPlan{Action: action, Revision: reg.Revision, Instances: []PlannedInstance{}}
	if action == "down" {
		return g.down(ctx, selections, cascade)
	}
	if action == "restart" {
		g.replace = map[string]bool{}
		for _, s := range selections {
			id, err := selectionID(s)
			if err != nil {
				return GraphPlan{}, err
			}
			g.replace[id] = true
		}
	}
	for _, s := range selections {
		if e = g.walk(s); e != nil {
			return GraphPlan{}, e
		}
	}
	if e = g.finalize(); e != nil {
		return GraphPlan{}, e
	}
	for _, id := range g.order {
		result.Instances = append(result.Instances, g.nodes[id].plan)
	}
	return result, nil
}
func (g *graphPlanner) finalize() error {
	for id := range g.reg.Projects {
		p, err := g.reg.Project(id)
		if err != nil {
			return err
		}
		for _, i := range p.Instances {
			for _, v := range i.Reservations {
				g.used[v] = true
			}
		}
	}
	for _, id := range g.order {
		n := g.nodes[id]
		if !n.live || g.replace[id] {
			if p, ok := n.service.Port.(int64); ok {
				if g.used[int(p)] && !(g.replace[id] && n.saved.Reservations[n.saved.RunID] == int(p)) {
					return &Error{"PORT_CONFLICT", fmt.Errorf("port %d is already reserved", p)}
				}
				g.used[int(p)] = true
			}
		}
	}
	for _, id := range g.order {
		n := g.nodes[id]
		if n.live && !g.replace[id] {
			continue
		}
		n.plan.Addresses = map[string]string{"host": "127.0.0.1"}
		if n.service.Port != nil {
			port := fmt.Sprint(n.service.Port)
			if port == "auto" {
				candidate := 20000
				if g.replace[id] && n.saved.Reservations[n.saved.RunID] != 0 {
					candidate = n.saved.Reservations[n.saved.RunID]
					delete(g.used, candidate)
				}
				for candidate <= 65535 && g.used[candidate] {
					candidate++
				}
				if candidate > 65535 {
					return &Error{"PORT_CONFLICT", errors.New("no port candidate available")}
				}
				g.used[candidate] = true
				port = fmt.Sprint(candidate)
				n.plan.Addresses["tentative"] = "true"
			}
			n.plan.Addresses["port"] = port
			if n.service.Protocol == "http" {
				n.plan.Addresses["direct_url"] = "http://127.0.0.1:" + port
				n.plan.Addresses["url"] = n.plan.Addresses["direct_url"]
			}
		}
		n.plan.Steps = []string{"prepare", "build", "start", "readiness"}
		if n.live && g.replace[id] {
			n.plan.Steps = []string{"prepare", "build", "stop", "start", "readiness"}
		}
		if n.saved.Preparation != nil {
			n.plan.Steps = append([]string{"recover_preparation"}, n.plan.Steps...)
		}
	}
	for _, id := range g.order {
		n := g.nodes[id]
		if !n.live || g.desired || g.replace[id] {
			if e := g.environment(n); e != nil {
				return e
			}
		}
	}
	return nil
}
func (g *graphPlanner) environment(n *graphNode) error {
	values := map[string]string{"worktree": n.selection.Worktree.Root, "project": n.selection.Project.Name, "service": n.selection.Service, "run_dir": "<run_dir>"}
	env := map[string]string{}
	for k, v := range n.plan.Addresses {
		if k != "tentative" {
			values[k] = v
			env["RDR_"+strings.ToUpper(k)] = v
		}
	}
	for dep, id := range n.plan.Bindings {
		target := g.nodes[id]
		norm := strings.ToUpper(strings.ReplaceAll(dep, "-", "_"))
		for k, v := range target.plan.Addresses {
			if k != "tentative" {
				values["dependencies."+dep+"."+k] = v
				env["RDR_DEP_"+norm+"_"+strings.ToUpper(k)] = v
			}
		}
	}
	for k, v := range n.service.Env {
		x, _, e := substitute(v, values, false)
		if e != nil {
			return &Error{"CONFIG_INVALID", e}
		}
		env[k] = x
	}
	for _, cmd := range append([]string{n.service.Build, n.service.Command}, mapValues(n.service.Hooks)...) {
		if _, _, e := substitute(cmd, values, true); e != nil {
			return &Error{"CONFIG_INVALID", e}
		}
	}
	n.plan.env = env
	return nil
}
func mapValues(m map[string]string) []string {
	v := make([]string, 0, len(m))
	for _, x := range m {
		v = append(v, x)
	}
	return v
}
func (g *graphPlanner) down(ctx context.Context, selections []Selection, cascade bool) (GraphPlan, error) {
	selected := map[string]bool{}
	projects := map[string]state.Project{}
	for _, s := range selections {
		id, e := selectionID(s)
		if e != nil {
			return GraphPlan{}, e
		}
		if _, e = g.load(s, false); e != nil {
			return GraphPlan{}, e
		}
		selected[id] = true
		p, e := g.reg.Project(s.Project.ID)
		if e != nil {
			return GraphPlan{}, e
		}
		projects[s.Project.ID] = p
	}
	for pid, p := range projects {
		for id, i := range p.Instances {
			if g.nodes[id] != nil {
				continue
			}
			var w state.Worktree
			for _, x := range p.Worktrees {
				if x.ID == i.WorktreeID {
					w = x
					break
				}
			}
			s := Selection{&discovery.Project{ID: pid}, &discovery.Worktree{ID: w.ID, Root: w.Root}, i.Service}
			if _, e := g.load(s, false); e != nil {
				return GraphPlan{}, e
			}
		}
	}
	changed := true
	for changed {
		changed = false
		ids := sortedNodes(g.nodes)
		for _, id := range ids {
			n := g.nodes[id]
			if selected[id] {
				continue
			}
			bindings := n.plan.Bindings
			if n.saved.Preparation != nil {
				m, err := g.r.Store.ReadManifest(n.saved.Preparation.RunID)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						return GraphPlan{}, &Error{"BUSY", fmt.Errorf("consumer %s has an unverified preparation", id)}
					}
					return GraphPlan{}, err
				}
				if m.InstanceID != id {
					return GraphPlan{}, fmt.Errorf("%w: preparation manifest mismatch", state.ErrCorrupt)
				}
				bindings = make(map[string]string, len(n.plan.Bindings)+len(m.EffectiveBindings))
				for k, v := range n.plan.Bindings {
					bindings[k] = v
				}
				for k, v := range m.EffectiveBindings {
					bindings["preparation:"+k] = v
				}
				n.plan.Bindings = bindings
			}
			if !n.live && n.saved.Preparation == nil {
				continue
			}
			for _, target := range bindings {
				if selected[target] {
					if !cascade {
						return GraphPlan{}, &Error{"STATE_CONFLICT", fmt.Errorf("live consumer %s depends on selected instance %s", id, target)}
					}
					selected[id] = true
					changed = true
					break
				}
			}
		}
	}
	var visit func(string) error
	visit = func(id string) error {
		if g.marks[id] == 1 {
			return &Error{"CYCLE", errors.New("effective dependency cycle")}
		}
		if g.marks[id] == 2 {
			return nil
		}
		g.marks[id] = 1
		n := g.nodes[id]
		deps := make([]string, 0, len(n.plan.Bindings))
		for _, v := range n.plan.Bindings {
			if selected[v] {
				deps = append(deps, v)
			}
		}
		sort.Strings(deps)
		for _, v := range deps {
			if e := visit(v); e != nil {
				return e
			}
		}
		g.marks[id] = 2
		g.order = append(g.order, id)
		return nil
	}
	for _, id := range sortedNodes(g.nodes) {
		if selected[id] {
			if e := visit(id); e != nil {
				return GraphPlan{}, e
			}
		}
	}
	out := GraphPlan{Action: "down", Revision: g.reg.Revision, Instances: []PlannedInstance{}}
	for i := len(g.order) - 1; i >= 0; i-- {
		n := g.nodes[g.order[i]]
		if n.live {
			n.plan.Steps = []string{"stop"}
		} else {
			n.plan.Steps = []string{}
		}
		if n.saved.Preparation != nil {
			n.plan.Steps = append([]string{"recover_preparation"}, n.plan.Steps...)
		}
		out.Instances = append(out.Instances, n.plan)
	}
	return out, ctx.Err()
}
func sortedNodes(m map[string]*graphNode) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// PlanBind validates a binding without writing state or taking operation locks.
func (r *Runner) PlanBind(ctx context.Context, consumer Selection, dependency string, target Selection) (BindResult, error) {
	result, _, _, err := r.planBind(ctx, consumer, dependency, target)
	return result, err
}
func (r *Runner) planBind(ctx context.Context, consumer Selection, dependency string, target Selection) (BindResult, uint64, bool, error) {
	cid, e := selectionID(consumer)
	if e != nil {
		return BindResult{}, 0, false, e
	}
	tid, e := selectionID(target)
	if e != nil {
		return BindResult{}, 0, false, e
	}
	if r == nil || r.Store == nil || consumer.Project.ID != target.Project.ID || target.Service != dependency {
		return BindResult{}, 0, false, &Error{"BINDING_INVALID", errors.New("target must be the declared service in the same project")}
	}
	if e = ctx.Err(); e != nil {
		return BindResult{}, 0, false, e
	}
	reg, e := r.Store.Read()
	if e != nil {
		return BindResult{}, 0, false, e
	}
	g := newGraph(r, reg)
	g.desired = true
	g.override = map[string]map[string]string{cid: {dependency: tid}}
	n, e := g.load(consumer, true)
	if e != nil {
		return BindResult{}, 0, false, e
	}
	declared := false
	for _, d := range n.service.DependsOn {
		if d == dependency {
			declared = true
		}
	}
	if !declared {
		return BindResult{}, 0, false, &Error{"BINDING_INVALID", fmt.Errorf("%s is not a declared dependency", dependency)}
	}
	if _, e = g.load(target, true); e != nil {
		return BindResult{}, 0, false, e
	}
	if e = g.walk(consumer); e != nil {
		return BindResult{}, 0, false, e
	}
	if e = g.finalize(); e != nil {
		return BindResult{}, 0, false, e
	}
	effective := n.plan.Bindings[dependency]
	if n.live {
		m, err := r.Store.ReadManifest(n.saved.RunID)
		if err != nil {
			return BindResult{}, 0, false, err
		}
		effective = m.EffectiveBindings[dependency]
		if effective == "" {
			effective = consumer.Project.ID + "/" + consumer.Worktree.ID + "/" + dependency
		}
	}
	return BindResult{ConsumerID: cid, TargetID: tid, RestartRequired: n.live && effective != tid}, reg.Revision, n.saved.DesiredBindings[dependency] == tid, nil
}

// Bind serializes the consumer operation and retries if the registry changed.
func (r *Runner) Bind(ctx context.Context, consumer Selection, dependency string, target Selection) (BindResult, error) {
	cid, e := selectionID(consumer)
	if e != nil {
		return BindResult{}, e
	}
	if r == nil || r.Store == nil {
		return BindResult{}, &Error{"USAGE", errors.New("missing runner")}
	}
	locks, e := r.Store.LockInstances(cid)
	if e != nil {
		if errors.Is(e, state.ErrBusy) {
			return BindResult{}, &Error{"BUSY", e}
		}
		return BindResult{}, e
	}
	defer locks.Close()
	for {
		result, revision, unchanged, err := r.planBind(ctx, consumer, dependency, target)
		if err != nil {
			return BindResult{}, err
		}
		if unchanged {
			return result, nil
		}
		conflict := errors.New("registry revision changed")
		err = r.Store.Update(func(current *state.Registry) error {
			if current.Revision != revision {
				return conflict
			}
			p, err := current.Project(consumer.Project.ID)
			if err != nil {
				return err
			}
			i, ok := p.Instances[cid]
			if !ok {
				return os.ErrNotExist
			}
			if i.DesiredBindings == nil {
				i.DesiredBindings = map[string]string{}
			}
			i.DesiredBindings[dependency] = result.TargetID
			p.Instances[cid] = i
			return current.SetProject(consumer.Project.ID, p)
		})
		if errors.Is(err, conflict) {
			continue
		}
		return result, err
	}
}
