// Package runner plans and executes lifecycle operations.
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

type Runner struct {
	Store      *state.Store
	Executable string
}

func New(store *state.Store, executable string) *Runner {
	return &Runner{Store: store, Executable: executable}
}

type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

type Plan struct {
	Action     string            `json:"action"`
	InstanceID string            `json:"instance_id"`
	State      string            `json:"state"`
	RunID      string            `json:"run_id,omitempty"`
	Digest     string            `json:"digest"`
	Revision   uint64            `json:"revision"`
	Addresses  map[string]string `json:"addresses,omitempty"`
	Steps      []string          `json:"steps"`
}

type Result struct {
	InstanceID string            `json:"instance_id"`
	State      string            `json:"state"`
	RunID      string            `json:"run_id,omitempty"`
	Status     string            `json:"status"`
	URLs       map[string]string `json:"urls,omitempty"`
	LogPaths   map[string]string `json:"log_paths,omitempty"`
}

// Plan reads the current configuration and registry without creating state files.
func (r *Runner) Plan(ctx context.Context, action string, project *discovery.Project, worktree *discovery.Worktree, service string) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, &Error{"CANCELLED", err}
	}
	if action != "up" && action != "down" && action != "restart" {
		return Plan{}, &Error{"USAGE", errors.New("unsupported lifecycle action")}
	}
	if r == nil || r.Store == nil || project == nil || worktree == nil {
		return Plan{}, &Error{"USAGE", errors.New("missing runner or selection")}
	}
	if action == "down" {
		return r.planDown(project, worktree, service)
	}
	fresh, err := discovery.Discover(worktree.Root)
	if err != nil {
		return Plan{}, err
	}
	if fresh.ID != project.ID {
		return Plan{}, &Error{"STATE_CONFLICT", errors.New("project identity changed")}
	}
	current, err := discovery.Resolve(fresh, worktree.ID)
	if err != nil {
		return Plan{}, err
	}
	if current.Root != worktree.Root {
		return Plan{}, &Error{"STATE_CONFLICT", errors.New("worktree identity changed")}
	}
	s, ok := current.Config.Services[service]
	if !ok {
		return Plan{}, &Error{"NOT_FOUND", fmt.Errorf("service %q not found", service)}
	}
	if len(s.DependsOn) != 0 || len(s.Hooks) != 0 {
		return Plan{}, &Error{"CONFIG_INVALID", errors.New("dependencies and hooks are not supported by this stage")}
	}
	reg, err := r.Store.Read()
	if err != nil {
		return Plan{}, err
	}
	saved, err := reg.Project(fresh.ID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Plan{}, &Error{"NOT_FOUND", errors.New("project is not registered")}
		}
		return Plan{}, err
	}
	id := fresh.ID + "/" + current.ID + "/" + service
	instance, ok := saved.Instances[id]
	if !ok {
		return Plan{}, &Error{"NOT_FOUND", errors.New("service is not registered")}
	}
	observed, err := r.Store.Inspect(instance)
	if err != nil {
		return Plan{}, err
	}
	p := Plan{Action: action, InstanceID: id, State: observed, RunID: instance.RunID, Digest: current.Config.Digest, Revision: reg.Revision, Steps: []string{}}
	if action != "down" && s.Port != nil {
		port := fmt.Sprint(s.Port)
		if port == "auto" {
			used := map[int]bool{}
			for id := range reg.Projects {
				project, err := reg.Project(id)
				if err != nil {
					return Plan{}, err
				}
				for _, instance := range project.Instances {
					for _, reserved := range instance.Reservations {
						used[reserved] = true
					}
				}
			}
			candidate := 20000
			if action == "restart" {
				if previous := instance.Reservations[instance.RunID]; previous != 0 {
					candidate = previous
					delete(used, previous)
				}
			}
			for candidate <= 65535 && used[candidate] {
				candidate++
			}
			if candidate > 65535 {
				return Plan{}, &Error{"PORT_CONFLICT", errors.New("no port candidate available")}
			}
			port = fmt.Sprint(candidate)
		}
		p.Addresses = map[string]string{"host": "127.0.0.1", "port": port, "tentative": "true"}
		if s.Protocol == "http" {
			p.Addresses["direct_url"] = "http://127.0.0.1:" + port
			p.Addresses["url"] = p.Addresses["direct_url"]
		}
	}
	switch action {
	case "up":
		if observed != "running" && observed != "ready" && observed != "unhealthy" {
			p.Steps = []string{"prepare", "build", "start", "readiness"}
		}
	case "down":
		if observed != "stopped" {
			p.Steps = []string{"stop"}
		}
	case "restart":
		p.Steps = []string{"prepare", "build", "stop", "start", "readiness"}
	}
	return p, nil
}

func (r *Runner) planDown(project *discovery.Project, worktree *discovery.Worktree, service string) (Plan, error) {
	reg, err := r.Store.Read()
	if err != nil {
		return Plan{}, err
	}
	saved, err := reg.Project(project.ID)
	if err != nil {
		return Plan{}, err
	}
	known := false
	for _, w := range saved.Worktrees {
		if w.ID == worktree.ID && w.Root == worktree.Root {
			known = true
			break
		}
	}
	if !known {
		return Plan{}, &Error{"NOT_FOUND", errors.New("worktree is not registered")}
	}
	id := project.ID + "/" + worktree.ID + "/" + service
	instance, ok := saved.Instances[id]
	if !ok {
		return Plan{}, &Error{"NOT_FOUND", errors.New("service is not registered")}
	}
	observed, err := r.Store.Inspect(instance)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{Action: "down", InstanceID: id, State: observed, RunID: instance.RunID, Revision: reg.Revision, Steps: []string{}}
	if instance.RunID != "" {
		manifest, err := r.Store.ReadManifest(instance.RunID)
		if err != nil {
			return Plan{}, err
		}
		if len(manifest.StopContract.Hooks) > 0 {
			return Plan{}, &Error{"CONFIG_INVALID", errors.New("stop hooks are not supported by this stage")}
		}
		plan.Digest = manifest.Digest
		plan.Addresses = manifest.Addresses
		if observed != "stopped" {
			plan.Steps = []string{"stop"}
		}
	}
	return plan, nil
}
