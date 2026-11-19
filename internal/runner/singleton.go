// Plans singleton handoffs and locks service starts across linked worktrees.
package runner

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func (g *graphPlanner) singletonMappings() ([]SwitchMapping, error) {
	var mappings []SwitchMapping
	for _, id := range g.order {
		n := g.nodes[id]
		if n.live && !g.replace[id] && !g.selected[id] {
			continue
		}
		if !n.plan.singleton {
			continue
		}
		for _, other := range g.order {
			v := g.nodes[other].selection
			otherNode := g.nodes[other]
			if other != id && v.Project.ID == n.selection.Project.ID && v.Service == n.selection.Service && (!otherNode.live || g.replace[other] || g.selected[other]) {
				return nil, &Error{"BINDING_INVALID", fmt.Errorf("singleton service %s requested in multiple worktrees", v.Service)}
			}
		}
		p, err := g.reg.Project(n.selection.Project.ID)
		if err != nil {
			return nil, err
		}
		var source *Selection
		for other, inst := range p.Instances {
			if other == id || inst.Service != n.selection.Service {
				continue
			}
			observed, err := g.r.Store.Inspect(inst)
			if err != nil {
				return nil, err
			}
			if observed == "stopped" && inst.Preparation == nil {
				continue
			}
			if !liveState(observed) || observed == "starting" || inst.Preparation != nil {
				if inst.RunID != "" || inst.Preparation != nil {
					return nil, &Error{"OWNERSHIP_UNKNOWN", fmt.Errorf("singleton instance %s requires explicit down/recovery", other)}
				}
				continue
			}
			if source != nil || n.live {
				return nil, &Error{"BINDING_INVALID", fmt.Errorf("singleton service %s has multiple active instances", inst.Service)}
			}
			for _, w := range p.Worktrees {
				if w.ID == inst.WorktreeID {
					source = &Selection{n.selection.Project, &discovery.Worktree{ID: w.ID, Root: w.Root}, inst.Service}
					break
				}
			}
			if source == nil {
				return nil, &Error{"STATE_CONFLICT", errors.New("singleton source worktree missing")}
			}
		}
		if source != nil {
			mappings = append(mappings, SwitchMapping{From: *source, To: n.selection})
		}
	}
	sort.Slice(mappings, func(i, j int) bool {
		a, _ := selectionID(mappings[i].From)
		b, _ := selectionID(mappings[j].From)
		return a < b
	})
	return mappings, nil
}

func (r *Runner) lockServiceStarts(instances []PlannedInstance) (*state.Locks, error) {
	groups := map[string]bool{}
	for _, instance := range instances {
		if !instance.singleton {
			continue
		}
		parts := strings.Split(instance.InstanceID, "/")
		if len(parts) != 3 {
			return nil, &Error{"STATE_CORRUPT", errors.New("invalid instance id")}
		}
		groups["service-start/"+parts[0]+"/"+parts[2]] = true
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	locks, err := r.Store.LockInstances(keys...)
	if errors.Is(err, state.ErrBusy) {
		return nil, &Error{"BUSY", err}
	}
	return locks, err
}
