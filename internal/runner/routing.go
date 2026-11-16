// Validates and reserves routes for service instances.
package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

type routeAssignment struct{ host, url, alias string }

func (r *Runner) assignRoutes(reg *state.Registry, selections map[string]Selection) (map[string]routeAssignment, error) {
	projects := map[string]state.Project{}
	owners := map[string]string{}
	changedProjects := map[string]bool{}
	for _, sel := range selections {
		changedProjects[sel.Project.ID] = true
	}
	for id := range reg.Projects {
		p, err := reg.Project(id)
		if err != nil {
			return nil, err
		}
		projects[id] = p
		for _, inst := range p.Instances {
			for _, ep := range inst.Endpoints {
				if ep.InstanceID != inst.ID {
					return nil, &Error{"STATE_CONFLICT", errors.New("invalid endpoint owner")}
				}
				if ep.Alias && changedProjects[id] {
					continue
				}
				if other := owners[ep.Hostname]; other != "" && other != inst.ID {
					return nil, &Error{"ROUTE_CONFLICT", fmt.Errorf("hostname %s already reserved", ep.Hostname)}
				}
				owners[ep.Hostname] = inst.ID
			}
			if inst.Hostname != "" && !(r.Routing != nil && r.Routing.LocalOnly && r.Routing.Legacy != nil && !strings.HasSuffix(inst.Hostname, ".localhost")) {
				host := strings.ToLower(inst.Hostname)
				if other := owners[host]; other != "" && other != inst.ID {
					return nil, &Error{"ROUTE_CONFLICT", fmt.Errorf("hostname %s already reserved; choose an alias", host)}
				}
				owners[host] = inst.ID
			}
		}
	}
	enabled := r.Routing != nil && r.Routing.Routing.Enabled
	ids := make([]string, 0, len(selections))
	for id := range selections {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	changes := map[string]routeAssignment{}
	for _, id := range ids {
		sel := selections[id]
		p, ok := projects[sel.Project.ID]
		if !ok {
			return nil, errors.New("project is not registered")
		}
		inst, ok := p.Instances[id]
		if !ok {
			return nil, errors.New("instance is not registered")
		}
		a := routeAssignment{host: inst.Hostname}
		if r.Routing != nil && r.Routing.LocalOnly && r.Routing.Legacy != nil {
			a.host = ""
			for _, ep := range inst.Endpoints {
				if ep.Scope == "local" && !ep.Alias && ep.InstanceID == id {
					a.host = ep.Hostname
					break
				}
			}
		}
		if !enabled {
			changes[id] = a
			continue
		}
		if sel.Worktree.Config == nil {
			if a.host == "" {
				return nil, &Error{"CONFIG_INVALID", errors.New("worktree configuration unavailable")}
			}
		} else {
			svc, ok := sel.Worktree.Config.Services[sel.Service]
			if !ok {
				return nil, &Error{"NOT_FOUND", errors.New("service configuration unavailable")}
			}
			if svc.Protocol != "http" {
				changes[id] = a
				continue
			}
		}
		if r.Routing.LocalOnly && !strings.HasSuffix(a.host, ".localhost") {
			a.host = ""
		}
		var wi int = -1
		for i, w := range p.Worktrees {
			if w.ID == sel.Worktree.ID && w.Root == sel.Worktree.Root {
				wi = i
				break
			}
		}
		if wi < 0 {
			return nil, &Error{"STATE_CONFLICT", errors.New("worktree changed")}
		}
		a.alias = p.Worktrees[wi].Alias
		if a.alias == "" {
			a.alias = filepath.Base(p.Worktrees[wi].Root)
		}
		if a.host == "" {
			domain := ""
			if !r.Routing.LocalOnly && sel.Worktree.Config.Routing != nil {
				domain = sel.Worktree.Config.Routing.Domain
			}
			var err error
			a.host, err = r.Routing.Hostname(sel.Service, a.alias, p.Name, domain)
			if err != nil {
				return nil, &Error{"CONFIG_INVALID", err}
			}
		}
		if a.host != strings.ToLower(a.host) {
			return nil, &Error{"CONFIG_INVALID", errors.New("stored hostname must be lowercase")}
		}
		if owner := owners[a.host]; owner != "" && owner != id {
			return nil, &Error{"ROUTE_CONFLICT", fmt.Errorf("hostname %s already reserved; choose an alias", a.host)}
		}
		owners[a.host] = id
		var err error
		a.url, err = r.Routing.URL(a.host)
		if err != nil {
			return nil, &Error{"CONFIG_INVALID", err}
		}
		changes[id] = a
	}
	if enabled && len(owners) > 0 {
		hosts := make([]string, 0, len(owners))
		for host := range owners {
			hosts = append(hosts, host)
		}
		sort.Strings(hosts)
		if err := routing.ValidateHosts(*r.Routing, hosts); err != nil {
			var te *routing.Error
			if errors.As(err, &te) {
				return nil, &Error{te.Code, te.Err}
			}
			return nil, err
		}
	}
	for _, id := range ids {
		sel := selections[id]
		p := projects[sel.Project.ID]
		inst := p.Instances[id]
		a := changes[id]
		if r.Routing == nil || !r.Routing.LocalOnly || a.url != "" {
			if r.Routing == nil || !r.Routing.LocalOnly || r.Routing.Legacy == nil || inst.Hostname == "" || strings.HasSuffix(inst.Hostname, ".localhost") {
				inst.Hostname = a.host
				inst.RouteURL = a.url
			}
		}
		if r.Routing != nil && r.Routing.LocalOnly && a.host != "" && a.url != "" {
			_, portText, _ := net.SplitHostPort(r.Routing.Routing.HTTPListen)
			port, _ := strconv.Atoi(portText)
			var endpoints []state.Endpoint
			for _, endpoint := range inst.Endpoints {
				if endpoint.Scope != "local" {
					endpoints = append(endpoints, endpoint)
				}
			}
			inst.Endpoints = append(endpoints, state.Endpoint{Scope: "local", Hostname: a.host, Port: port, InstanceID: id})
			if r.Routing.TailnetDomain != "" && r.Routing.LocalProjects[sel.Project.ID].Tailnet {
				hostname := strings.TrimSuffix(a.host, ".localhost") + "." + r.Routing.TailnetDomain
				if owner := owners[hostname]; owner != "" && owner != id {
					return nil, &Error{"ROUTE_CONFLICT", fmt.Errorf("hostname %s already reserved", hostname)}
				}
				owners[hostname] = id
				var filtered []state.Endpoint
				for _, ep := range inst.Endpoints {
					if ep.Scope != "tailnet" {
						filtered = append(filtered, ep)
					}
				}
				inst.Endpoints = append(filtered, state.Endpoint{Scope: "tailnet", Hostname: hostname, Port: 443, InstanceID: id})
			} else {
				var filtered []state.Endpoint
				for _, ep := range inst.Endpoints {
					if ep.Scope != "tailnet" {
						filtered = append(filtered, ep)
					}
				}
				inst.Endpoints = filtered
			}
		}
		p.Instances[id] = inst
		if a.alias != "" {
			for i := range p.Worktrees {
				if p.Worktrees[i].ID == sel.Worktree.ID && p.Worktrees[i].Alias == "" {
					p.Worktrees[i].Alias = a.alias
				}
			}
		}
		projects[p.ID] = p
	}
	if r.Routing != nil && r.Routing.LocalOnly {
		if err := r.assignLocalAliases(projects, selections, owners); err != nil {
			return nil, err
		}
	}
	touched := map[string]bool{}
	for _, sel := range selections {
		touched[sel.Project.ID] = true
	}
	for id := range touched {
		p := projects[id]
		if err := reg.SetProject(id, p); err != nil {
			return nil, err
		}
	}
	return changes, nil
}

func (r *Runner) AssignProjectRoutes(reg *state.Registry, fresh *discovery.Project) error {
	if r == nil || reg == nil || fresh == nil {
		return &Error{"USAGE", errors.New("missing routing input")}
	}
	saved, err := reg.Project(fresh.ID)
	if err != nil {
		return err
	}
	selections := map[string]Selection{}
	for i := range fresh.Worktrees {
		w := &fresh.Worktrees[i]
		if w.Config == nil {
			continue
		}
		for name := range w.Config.Services {
			id := fresh.ID + "/" + w.ID + "/" + name
			if instance, ok := saved.Instances[id]; ok {
				selectedWorktree := w
				if r.Routing != nil && r.Routing.Routing.Enabled && instance.Hostname != "" && instance.RunID != "" && (!r.Routing.LocalOnly || r.Routing.Legacy == nil || hasLocalEndpoint(instance)) {
					observed, e := r.Store.Inspect(instance)
					if e != nil {
						return e
					}
					if liveState(observed) {
						manifest, e := r.Store.ReadManifest(instance.RunID)
						if e != nil {
							return e
						}
						if manifest.InstanceID != id {
							return fmt.Errorf("%w: route manifest mismatch", state.ErrCorrupt)
						}
						if manifest.Launch.Protocol == "http" {
							copy := *w
							copy.Config = nil
							selectedWorktree = &copy
						}
					}
				}
				selections[id] = Selection{fresh, selectedWorktree, name}
			}
		}
	}
	_, err = r.assignRoutes(reg, selections)
	return err
}

func (r *Runner) ReservePlannedRoutes(reg *state.Registry, plan GraphPlan) error {
	selections := make(map[string]Selection, len(plan.Instances))
	for _, p := range plan.Instances {
		if p.routeSelection.Project == nil || p.routeSelection.Worktree == nil {
			return &Error{"STATE_CONFLICT", errors.New("plan lacks route selection")}
		}
		selections[p.InstanceID] = p.routeSelection
	}
	candidate := *reg
	candidate.Projects = make(map[string]json.RawMessage, len(reg.Projects))
	for id, data := range reg.Projects {
		candidate.Projects[id] = data
	}
	routes, err := r.assignRoutes(&candidate, selections)
	if err != nil {
		return err
	}
	for _, p := range plan.Instances {
		if routes[p.InstanceID] != p.route {
			return &Error{"STATE_CONFLICT", errors.New("route changed during planning")}
		}
	}
	reg.Projects = candidate.Projects
	return nil
}

func hasLocalEndpoint(instance state.Instance) bool {
	for _, ep := range instance.Endpoints {
		if ep.Scope == "local" && !ep.Alias && ep.InstanceID == instance.ID {
			return true
		}
	}
	return false
}
