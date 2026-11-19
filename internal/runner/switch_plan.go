// Plans service worktree switches and affected dependency changes.
package runner

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

type SwitchMapping struct {
	From Selection
	To   Selection
}
type SwitchPair struct {
	From string `json:"from"`
	To   string `json:"to"`
}
type BindingRewrite struct {
	ConsumerID string `json:"consumer_id"`
	Dependency string `json:"dependency"`
	From       string `json:"from"`
	To         string `json:"to"`
}
type SwitchStep struct {
	ID         string   `json:"id"`
	Action     string   `json:"action"`
	InstanceID string   `json:"instance_id"`
	DependsOn  []string `json:"depends_on,omitempty"`
}
type SwitchPlan struct {
	Version         int               `json:"version"`
	Action          string            `json:"action"`
	Revision        uint64            `json:"revision"`
	Mappings        []SwitchPair      `json:"mappings"`
	Locks           []string          `json:"locks"`
	Graph           []PlannedInstance `json:"graph"`
	Digests         map[string]string `json:"digests"`
	OldStops        []PlannedInstance `json:"old_stops"`
	NewStarts       []PlannedInstance `json:"new_starts"`
	Preparations    []string          `json:"preparations"`
	BindingRewrites []BindingRewrite  `json:"binding_rewrites"`
	Steps           []SwitchStep      `json:"steps"`
	Publications    []string          `json:"publications,omitempty"`
}

func (r *Runner) PlanSwitch(ctx context.Context, mappings []SwitchMapping) (SwitchPlan, error) {
	return r.planSwitch(ctx, mappings, nil, nil)
}

func (r *Runner) planSwitch(ctx context.Context, mappings []SwitchMapping, starts []Selection, snapshot *state.Registry) (SwitchPlan, error) {
	if err := ctx.Err(); err != nil {
		return SwitchPlan{}, &Error{"CANCELLED", err}
	}
	if r == nil || r.Store == nil || len(mappings) == 0 {
		return SwitchPlan{}, &Error{"USAGE", errors.New("missing switch mappings")}
	}
	var reg state.Registry
	var err error
	if snapshot != nil {
		reg = *snapshot
	} else {
		reg, err = r.Store.Read()
		if err != nil {
			return SwitchPlan{}, err
		}
	}
	out := SwitchPlan{Version: 1, Action: "switch", Revision: reg.Revision, Digests: map[string]string{}, Mappings: []SwitchPair{}, Locks: []string{}, OldStops: []PlannedInstance{}, NewStarts: []PlannedInstance{}, Preparations: []string{}, BindingRewrites: []BindingRewrite{}, Steps: []SwitchStep{}}
	info := newGraph(r, reg)
	mapped := map[string]string{}
	targets := map[string]bool{}
	selections := map[string]Selection{}
	projectIDs := map[string]bool{}
	for _, m := range mappings {
		from, e := selectionID(m.From)
		if e != nil {
			return SwitchPlan{}, e
		}
		to, e := selectionID(m.To)
		if e != nil {
			return SwitchPlan{}, e
		}
		if m.From.Project.ID != m.To.Project.ID || m.From.Service != m.To.Service || from == to || mapped[from] != "" || targets[to] {
			return SwitchPlan{}, &Error{"BINDING_INVALID", errors.New("invalid or duplicate switch mapping")}
		}
		mapped[from] = to
		targets[to] = true
		selections[from] = m.From
		selections[to] = m.To
		projectIDs[m.From.Project.ID] = true
		out.Mappings = append(out.Mappings, SwitchPair{from, to})
	}
	for id := range mapped {
		if targets[id] {
			return SwitchPlan{}, &Error{"BINDING_INVALID", errors.New("source and target sets overlap")}
		}
	}
	for _, s := range starts {
		id, err := selectionID(s)
		if err != nil {
			return SwitchPlan{}, err
		}
		if mapped[id] != "" {
			return SwitchPlan{}, &Error{"BINDING_INVALID", errors.New("cannot start a stopping source")}
		}
		selections[id] = s
		projectIDs[s.Project.ID] = true
	}

	projects := map[string]state.Project{}
	for pid := range projectIDs {
		p, e := reg.Project(pid)
		if e != nil {
			return SwitchPlan{}, e
		}
		projects[pid] = p
		for id, i := range p.Instances {
			if _, ok := selections[id]; !ok {
				for _, w := range p.Worktrees {
					if w.ID == i.WorktreeID {
						selections[id] = Selection{&discovery.Project{ID: pid}, &discovery.Worktree{ID: w.ID, Root: w.Root}, i.Service}
						break
					}
				}
			}
			if _, e = info.load(selections[id], false); e != nil {
				return SwitchPlan{}, e
			}
		}
	}

	validate := newGraph(r, reg)
	validate.desired = true
	validate.replace = map[string]bool{}
	for from, to := range mapped {
		validate.replace[from] = true
		validate.replace[to] = true
	}
	for _, m := range out.Mappings {
		a, e := validate.load(selections[m.From], true)
		if e != nil {
			return SwitchPlan{}, e
		}
		b, e := validate.load(selections[m.To], true)
		if e != nil {
			return SwitchPlan{}, e
		}
		if !a.live || a.plan.State == "starting" {
			return SwitchPlan{}, &Error{"BINDING_INVALID", fmt.Errorf("source %s is not ready", m.From)}
		}
		if b.live || b.saved.Preparation != nil {
			return SwitchPlan{}, &Error{"BINDING_INVALID", fmt.Errorf("target %s is active", m.To)}
		}
	}
	affected := map[string]bool{}
	for id := range mapped {
		affected[id] = true
	}
	changed := true
	for changed {
		changed = false
		for id, n := range info.nodes {
			if !n.live || affected[id] {
				continue
			}
			for _, target := range n.plan.Bindings {
				if affected[target] {
					affected[id] = true
					changed = true
					break
				}
			}
		}
	}

	for id := range affected {
		n, e := validate.load(selections[id], true)
		if e != nil {
			return SwitchPlan{}, e
		}
		effective := info.nodes[id].plan.Bindings
		for dep := range n.saved.DesiredBindings {
			declared := false
			for _, name := range n.service.DependsOn {
				if dep == name {
					declared = true
				}
			}
			if !declared {
				return SwitchPlan{}, &Error{"BINDING_INVALID", fmt.Errorf("undeclared desired binding %s for %s", dep, id)}
			}
		}
		if len(effective) != len(n.service.DependsOn) {
			return SwitchPlan{}, &Error{"BINDING_INVALID", fmt.Errorf("pending binding for %s", id)}
		}
		for _, dep := range n.service.DependsOn {
			target := n.saved.DesiredBindings[dep]
			if target == "" {
				target = n.selection.Project.ID + "/" + n.selection.Worktree.ID + "/" + dep
			}
			if effective[dep] != target {
				return SwitchPlan{}, &Error{"BINDING_INVALID", fmt.Errorf("pending binding for %s dependency %s", id, dep)}
			}
		}
	}
	replacements := map[string]string{}
	for id := range affected {
		if target := mapped[id]; target != "" {
			replacements[id] = target
		} else {
			replacements[id] = id
		}
	}

	overrides := map[string]map[string]string{}
	for source, target := range replacements {
		src := validate.nodes[source]
		dst, e := validate.load(selections[target], true)
		if e != nil {
			return SwitchPlan{}, e
		}
		overrides[target] = map[string]string{}
		if source != target {
			for dep, value := range dst.saved.DesiredBindings {
				declared := false
				for _, name := range dst.service.DependsOn {
					if name == dep {
						declared = true
					}
				}
				if !declared {
					return SwitchPlan{}, &Error{"BINDING_INVALID", fmt.Errorf("binding %s is not declared by %s", dep, target)}
				}
				if mapped[value] != "" {
					return SwitchPlan{}, &Error{"BINDING_INVALID", fmt.Errorf("target override points to stopping source %s", value)}
				}
			}
		}
		for _, dep := range dst.service.DependsOn {
			v := dst.saved.DesiredBindings[dep]
			if source == target {
				if v == "" {
					v = source[:strings.LastIndex(source, "/")+1] + dep
				}
				if mapped[v] != "" {
					v = mapped[v]
				}
			} else if v != "" {
				if mapped[v] != "" {
					return SwitchPlan{}, &Error{"BINDING_INVALID", fmt.Errorf("target override points to stopping source %s", v)}
				}
			} else if source != target {
				v = src.saved.DesiredBindings[dep]
				if v != "" {
					if mapped[v] != "" {
						v = mapped[v]
					}
				} else {
					old := source[:strings.LastIndex(source, "/")+1] + dep
					if mapped[old] != "" {
						v = mapped[old]
					}
				}
			}
			if v == "" {
				v = target[:strings.LastIndex(target, "/")+1] + dep
			}
			overrides[target][dep] = v
			before := dst.saved.DesiredBindings[dep]
			defaultTarget := target[:strings.LastIndex(target, "/")+1] + dep
			inheritedExplicit := source != target && src.saved.DesiredBindings[dep] != ""
			if before != v && (before != "" || v != defaultTarget || inheritedExplicit) {
				out.BindingRewrites = append(out.BindingRewrites, BindingRewrite{target, dep, before, v})
			}
		}
	}
	for _, p := range projects {
		for id, inst := range p.Instances {
			if affected[id] || targets[id] || info.nodes[id].live {
				continue
			}
			for dep, value := range inst.DesiredBindings {
				if to := mapped[value]; to != "" {
					if overrides[id] == nil {
						overrides[id] = map[string]string{}
					}
					overrides[id][dep] = to
					out.BindingRewrites = append(out.BindingRewrites, BindingRewrite{id, dep, value, to})
				}
			}
		}
	}

	g := newGraph(r, reg)
	g.replace = map[string]bool{}
	g.retiring = affected
	g.override = overrides
	for _, id := range replacements {
		g.replace[id] = true
	}
	for id := range overrides {
		if !targets[id] && !affected[id] {
			g.replace[id] = true
		}
	}
	var roots []string
	for _, id := range replacements {
		roots = append(roots, id)
	}
	for _, s := range starts {
		id, _ := selectionID(s)
		roots = append(roots, id)
	}
	sort.Strings(roots)
	for _, id := range roots {
		if err = g.walk(selections[id]); err != nil {
			return SwitchPlan{}, err
		}
	}
	needed := map[string]bool{}
	for _, id := range g.order {
		needed[id] = true
	}

	for _, id := range g.order {
		for name, target := range g.nodes[id].plan.Bindings {
			if mapped[target] != "" {
				return SwitchPlan{}, &Error{"BINDING_INVALID", fmt.Errorf("needed instance %s dependency %s points to stopping source %s", id, name, target)}
			}
		}
	}
	var metadataRoots []string
	for id := range overrides {
		if !targets[id] && !affected[id] {
			metadataRoots = append(metadataRoots, id)
		}
	}
	sort.Strings(metadataRoots)
	for _, id := range metadataRoots {
		if err = g.walk(selections[id]); err != nil {
			return SwitchPlan{}, err
		}
	}
	for _, id := range metadataRoots {
		n := g.nodes[id]
		for dep := range overrides[id] {
			declared := false
			for _, name := range n.service.DependsOn {
				if name == dep {
					declared = true
				}
			}
			if !declared {
				return SwitchPlan{}, &Error{"BINDING_INVALID", fmt.Errorf("binding %s is not declared by %s", dep, id)}
			}
		}
	}

	desired := newGraph(r, reg)
	desired.desired = true
	desired.override = overrides
	desired.replace = g.replace
	for _, id := range append(append([]string{}, roots...), metadataRoots...) {
		if err = desired.walk(selections[id]); err != nil {
			return SwitchPlan{}, err
		}
	}

	var metadataOnly []*graphNode
	for id, n := range g.nodes {
		if !needed[id] && !n.live {
			metadataOnly = append(metadataOnly, n)
			n.live = true
			g.replace[id] = false
			n.plan.Addresses = map[string]string{"host": "127.0.0.1"}
			if n.service.Port != nil {
				n.plan.Addresses["port"] = "20000"
				if n.service.Protocol == "http" {
					n.plan.Addresses["direct_url"] = "http://127.0.0.1:20000"
					n.plan.Addresses["url"] = n.plan.Addresses["direct_url"]
				}
			}
		}
	}
	if err = g.finalize(); err != nil {
		return SwitchPlan{}, err
	}
	for _, n := range metadataOnly {
		if err = g.environment(n); err != nil {
			return SwitchPlan{}, err
		}
	}
	singletons, err := g.singletonMappings()
	if err != nil {
		return SwitchPlan{}, err
	}
	for _, pair := range singletons {
		from, _ := selectionID(pair.From)
		to, _ := selectionID(pair.To)
		if mapped[from] != to {
			return SwitchPlan{}, &Error{"BINDING_INVALID", errors.New("singleton dependency requires an additional switch mapping")}
		}
	}
	for id, n := range g.nodes {
		if !needed[id] && n.service.Command == "" {
			continue
		}
		for _, route := range n.service.Routes {
			target := g.nodes[n.plan.Bindings[route.Dependency]]
			if target == nil || target.plan.Addresses["direct_url"] == "" {
				return SwitchPlan{}, &Error{"CONFIG_INVALID", fmt.Errorf("route %s requires an HTTP dependency", route.Dependency)}
			}
		}
	}

	downSelections := make([]Selection, 0, len(affected))
	for id := range affected {
		downSelections = append(downSelections, selections[id])
	}
	sort.Slice(downSelections, func(i, j int) bool {
		a, _ := selectionID(downSelections[i])
		b, _ := selectionID(downSelections[j])
		return a < b
	})
	dg := newGraph(r, reg)
	down, err := dg.down(ctx, downSelections, false)
	if err != nil {
		return SwitchPlan{}, err
	}
	out.OldStops = down.Instances
	lockSet := map[string]bool{}
	for _, id := range g.order {
		lockSet[id] = true
		n := g.nodes[id]
		if needed[id] {
			out.Graph = append(out.Graph, n.plan)
		}
		if needed[id] && (!n.live || replacementsContains(replacements, id)) {
			if !affected[id] && n.plan.RunID != "" && n.plan.State != "stopped" {
				return SwitchPlan{}, &Error{"OWNERSHIP_UNKNOWN", fmt.Errorf("target %s requires explicit down/recovery", id)}
			}
			out.NewStarts = append(out.NewStarts, n.plan)
			out.Preparations = append(out.Preparations, id)
		}
	}
	var includeOldDeps func(string) error
	visitedOld := map[string]bool{}
	includeOldDeps = func(id string) error {
		if visitedOld[id] {
			return nil
		}
		n := info.nodes[id]
		if n == nil {
			return &Error{"BINDING_INVALID", fmt.Errorf("missing effective instance %s", id)}
		}
		visitedOld[id] = true
		lockSet[id] = true
		if n.live {
			for _, dep := range n.plan.Bindings {
				if e := includeOldDeps(dep); e != nil {
					return e
				}
			}
		}
		return nil
	}
	for id := range affected {
		if e := includeOldDeps(id); e != nil {
			return SwitchPlan{}, e
		}
	}

	for id := range overrides {
		lockSet[id] = true
	}
	for id := range lockSet {
		out.Locks = append(out.Locks, id)
	}
	sort.Strings(out.Locks)
	for _, nodes := range []map[string]*graphNode{validate.nodes, g.nodes, desired.nodes} {
		for id, n := range nodes {
			if n.selection.Worktree != nil && n.selection.Worktree.Config != nil {
				digest := n.selection.Worktree.Config.Digest
				if prior := out.Digests[id]; prior != "" && prior != digest {
					return SwitchPlan{}, &Error{"STATE_CONFLICT", fmt.Errorf("configuration changed while planning %s", id)}
				}
				out.Digests[id] = digest
			}
		}
	}
	sort.Slice(out.BindingRewrites, func(i, j int) bool {
		a, b := out.BindingRewrites[i], out.BindingRewrites[j]
		if a.ConsumerID == b.ConsumerID {
			return a.Dependency < b.Dependency
		}
		return a.ConsumerID < b.ConsumerID
	})
	for _, p := range out.NewStarts {
		deps := []string{}
		for _, id := range p.Bindings {
			if needed[id] && !g.nodes[id].live && !replacementsContains(replacements, id) {
				deps = append(deps, "start:"+id)
			}
		}
		sort.Strings(deps)
		out.Steps = append(out.Steps, SwitchStep{ID: "prepare:" + p.InstanceID, Action: "prepare", InstanceID: p.InstanceID, DependsOn: deps})
	}
	for _, p := range out.OldStops {
		deps := []string{}
		for _, id := range out.Preparations {
			deps = append(deps, "prepare:"+id)
		}
		for _, other := range out.OldStops {
			for _, target := range other.Bindings {
				if target == p.InstanceID {
					deps = append(deps, "stop:"+other.InstanceID)
				}
			}
		}
		sort.Strings(deps)
		out.Steps = append(out.Steps, SwitchStep{ID: "stop:" + p.InstanceID, Action: "stop", InstanceID: p.InstanceID, DependsOn: deps})
	}
	for _, rewrite := range out.BindingRewrites {
		deps := []string{}
		if needed[rewrite.To] && (!g.nodes[rewrite.To].live || replacementsContains(replacements, rewrite.To)) {
			deps = append(deps, "start:"+rewrite.To)
		}
		out.Steps = append(out.Steps, SwitchStep{ID: "bind:" + rewrite.ConsumerID + ":" + rewrite.Dependency, Action: "bind", InstanceID: rewrite.ConsumerID, DependsOn: deps})
	}
	for _, p := range out.NewStarts {
		deps := []string{"prepare:" + p.InstanceID}
		for _, rewrite := range out.BindingRewrites {
			if rewrite.ConsumerID == p.InstanceID {
				deps = append(deps, "bind:"+p.InstanceID+":"+rewrite.Dependency)
			}
		}
		for _, v := range p.Bindings {
			if needed[v] && (!g.nodes[v].live || replacementsContains(replacements, v)) {
				deps = append(deps, "start:"+v)
			}
		}
		for old, target := range replacements {
			if target == p.InstanceID {
				deps = append(deps, "stop:"+old)
			}
		}
		sort.Strings(deps)
		out.Steps = append(out.Steps, SwitchStep{ID: "start:" + p.InstanceID, Action: "start", InstanceID: p.InstanceID, DependsOn: deps})
	}
	ids := make([]string, 0, len(out.OldStops))
	for _, instance := range out.OldStops {
		ids = append(ids, instance.InstanceID)
	}
	out.Publications, err = publicationIDsForInstances(r.Store, ids)
	if err != nil {
		return SwitchPlan{}, err
	}
	return out, nil
}
func replacementsContains(m map[string]string, id string) bool {
	for _, v := range m {
		if v == id {
			return true
		}
	}
	return false
}
