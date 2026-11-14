package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

// LocalRoutePlan is an in-memory registry candidate tied to its source revision.
type LocalRouteChange struct {
	Action     string `json:"action"`
	ProjectID  string `json:"project_id"`
	InstanceID string `json:"instance_id"`
	Scope      string `json:"scope"`
	Hostname   string `json:"hostname"`
	Port       int    `json:"port"`
	Alias      bool   `json:"alias"`
}
type LocalRoutePlan struct {
	Revision  uint64             `json:"revision"`
	Changes   []LocalRouteChange `json:"changes"`
	candidate state.Registry
	changed   bool
}

// PlanLocalRoutes builds and validates routes without writing registry state.
func PlanLocalRoutes(ctx context.Context, store *state.Store, cfg routing.Config) (LocalRoutePlan, error) {
	if err := ctx.Err(); err != nil {
		return LocalRoutePlan{}, err
	}
	before, err := store.Read()
	if err != nil {
		return LocalRoutePlan{}, err
	}
	ids := make([]string, 0, len(before.Projects))
	for id := range before.Projects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	projects := make([]*discovery.Project, 0, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return LocalRoutePlan{}, err
		}
		saved, err := before.Project(id)
		if err != nil {
			return LocalRoutePlan{}, err
		}

		var fresh *discovery.Project
		for _, w := range saved.Worktrees {
			fresh, err = discovery.Discover(w.Root)
			if err == nil {
				break
			}
			if !os.IsNotExist(err) {
				return LocalRoutePlan{}, fmt.Errorf("discover %s: %w", w.Root, err)
			}
		}
		if fresh == nil {
			continue
		} // Deleted worktrees retain their owned, unavailable routes.
		if fresh.ID != id {
			return LocalRoutePlan{}, fmt.Errorf("project identity changed for %s", saved.Root)
		}
		projects = append(projects, fresh)
	}
	r := New(store, "")
	r.Routing = &cfg
	candidate := before
	candidate.Projects = make(map[string]json.RawMessage, len(before.Projects))
	for id, data := range before.Projects {
		candidate.Projects[id] = data
	}
	for _, project := range projects {
		if err := ctx.Err(); err != nil {
			return LocalRoutePlan{}, err
		}
		if err := r.AssignProjectRoutes(&candidate, project); err != nil {
			return LocalRoutePlan{}, err
		}
	}
	oldRoutes, err := routeSet(before)
	if err != nil {
		return LocalRoutePlan{}, err
	}
	newRoutes, err := routeSet(candidate)
	if err != nil {
		return LocalRoutePlan{}, err
	}
	changes := make([]LocalRouteChange, 0)
	for key, route := range oldRoutes {
		if _, ok := newRoutes[key]; !ok {
			route.Action = "remove"
			changes = append(changes, route)
		}
	}
	for key, route := range newRoutes {
		if _, ok := oldRoutes[key]; !ok {
			route.Action = "add"
			changes = append(changes, route)
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		a, b := changes[i], changes[j]
		return fmt.Sprintf("%s/%s/%s/%s/%05d/%t/%s", a.ProjectID, a.InstanceID, a.Scope, a.Hostname, a.Port, a.Alias, a.Action) < fmt.Sprintf("%s/%s/%s/%s/%05d/%t/%s", b.ProjectID, b.InstanceID, b.Scope, b.Hostname, b.Port, b.Alias, b.Action)
	})
	return LocalRoutePlan{Revision: before.Revision, Changes: changes, candidate: candidate, changed: len(projects) > 0}, nil
}

// CommitLocalRoutes atomically installs a validated candidate if the registry is unchanged.
func CommitLocalRoutes(ctx context.Context, store *state.Store, plan LocalRoutePlan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !plan.changed {
		current, err := store.Read()
		if err != nil {
			return err
		}
		if current.Revision != plan.Revision {
			return &Error{"STATE_CONFLICT", fmt.Errorf("registry changed during route reconciliation")}
		}
		return nil
	}
	return store.Update(func(reg *state.Registry) error {
		if reg.Revision != plan.candidate.Revision {
			return &Error{"STATE_CONFLICT", fmt.Errorf("registry changed during route reconciliation")}
		}
		reg.Projects = plan.candidate.Projects
		return nil
	})
}

// ReconcileLocalRoutes builds and commits a candidate, preserving existing runs.
func ReconcileLocalRoutes(ctx context.Context, store *state.Store, cfg routing.Config) error {
	plan, err := PlanLocalRoutes(ctx, store, cfg)
	if err != nil {
		return err
	}
	return CommitLocalRoutes(ctx, store, plan)
}

func routeSet(reg state.Registry) (map[string]LocalRouteChange, error) {
	routes := map[string]LocalRouteChange{}
	for id := range reg.Projects {
		project, err := reg.Project(id)
		if err != nil {
			return nil, err
		}
		for _, instance := range project.Instances {
			for _, ep := range instance.Endpoints {
				route := LocalRouteChange{ProjectID: id, InstanceID: instance.ID, Scope: ep.Scope, Hostname: ep.Hostname, Port: ep.Port, Alias: ep.Alias}
				key := fmt.Sprintf("%s/%s/%s/%s/%d/%t", id, instance.ID, ep.Scope, ep.Hostname, ep.Port, ep.Alias)
				routes[key] = route
			}
		}
	}
	return routes, nil
}
