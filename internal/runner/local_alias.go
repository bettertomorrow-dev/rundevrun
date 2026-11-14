package runner

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

// assignLocalAliases reserves the short project hostname only when both the
// main/master worktree and its HTTP service are unambiguous or explicitly set.
func (r *Runner) assignLocalAliases(projects map[string]state.Project, selections map[string]Selection, owners map[string]string) error {
	processed := map[string]bool{}
	for _, sel := range selections {
		projectID := sel.Project.ID
		if processed[projectID] {
			continue
		}
		processed[projectID] = true
		p := projects[projectID]
		choice := r.Routing.LocalProjects[projectID]
		var primary []string
		for _, w := range sel.Project.Worktrees {
			if (w.Branch == "main" || w.Branch == "master") && w.Config != nil {
				primary = append(primary, w.ID)
			}
		}
		worktreeID := choice.PrimaryWorktree
		if worktreeID == "" && len(primary) == 1 {
			worktreeID = primary[0]
		}
		validWorktree := false
		for _, id := range primary {
			if id == worktreeID {
				validWorktree = true
			}
		}
		service := choice.DefaultService
		var services []string
		if validWorktree {
			for _, w := range sel.Project.Worktrees {
				if w.ID == worktreeID && w.Config != nil {
					for name, spec := range w.Config.Services {
						if spec.Protocol == "http" {
							services = append(services, name)
						}
					}
				}
			}
		}
		if service == "" && len(services) == 1 {
			service = services[0]
		}
		validService := false
		for _, name := range services {
			if name == service {
				validService = true
			}
		}
		targetID := projectID + "/" + worktreeID + "/" + service
		target, exists := p.Instances[targetID]
		// Always remove stale aliases from this project before deriving a new one.
		for id, inst := range p.Instances {
			endpoints := inst.Endpoints[:0]
			for _, ep := range inst.Endpoints {
				if !ep.Alias || (ep.Scope != "local" && ep.Scope != "tailnet") {
					endpoints = append(endpoints, ep)
				}
			}
			inst.Endpoints = endpoints
			p.Instances[id] = inst
		}
		if validWorktree && validService && exists {
			// The hostname helper validates normalization and prevents dotted labels.
			hostname, err := r.Routing.Hostname("x", "x", p.Name, "localhost")
			if err != nil {
				return err
			}
			hostname = strings.TrimPrefix(hostname, "x-x-")
			if other := owners[hostname]; other != "" && other != targetID {
				return &Error{"ROUTE_CONFLICT", fmt.Errorf("hostname %s already reserved", hostname)}
			}
			owners[hostname] = targetID
			_, rawPort, _ := net.SplitHostPort(r.Routing.Routing.HTTPListen)
			port, _ := strconv.Atoi(rawPort)
			target = p.Instances[targetID]
			target.Endpoints = append(target.Endpoints, state.Endpoint{Scope: "local", Hostname: hostname, Port: port, InstanceID: targetID, Alias: true})
			if choice.Tailnet && r.Routing.TailnetDomain != "" {
				tailhost := strings.TrimSuffix(hostname, ".localhost") + "." + r.Routing.TailnetDomain
				if owner := owners[tailhost]; owner != "" && owner != targetID {
					return &Error{"ROUTE_CONFLICT", fmt.Errorf("hostname %s already reserved", tailhost)}
				}
				owners[tailhost] = targetID
				target.Endpoints = append(target.Endpoints, state.Endpoint{Scope: "tailnet", Hostname: tailhost, Port: 443, InstanceID: targetID, Alias: true})
			}
			p.Instances[targetID] = target
		}
		projects[projectID] = p
	}
	return nil
}

// setCanonicalEndpoint changes entry routes without modifying an existing run manifest.
func (r *Runner) setCanonicalEndpoint(inst *state.Instance, projectID, host, url string) {
	if !r.Routing.LocalOnly || r.Routing.Legacy == nil || inst.Hostname == "" || strings.HasSuffix(inst.Hostname, ".localhost") {
		inst.Hostname, inst.RouteURL = host, url
	}
	if !r.Routing.LocalOnly {
		return
	}
	_, rawPort, _ := net.SplitHostPort(r.Routing.Routing.HTTPListen)
	port, _ := strconv.Atoi(rawPort)
	var endpoints []state.Endpoint
	for _, ep := range inst.Endpoints {
		if ep.Alias || (ep.Scope != "local" && ep.Scope != "tailnet") {
			endpoints = append(endpoints, ep)
		}
	}
	endpoints = append(endpoints, state.Endpoint{Scope: "local", Hostname: host, Port: port, InstanceID: inst.ID})
	if r.Routing.TailnetDomain != "" && r.Routing.LocalProjects[projectID].Tailnet {
		endpoints = append(endpoints, state.Endpoint{Scope: "tailnet", Hostname: strings.TrimSuffix(host, ".localhost") + "." + r.Routing.TailnetDomain, Port: 443, InstanceID: inst.ID})
	}
	inst.Endpoints = endpoints
}
