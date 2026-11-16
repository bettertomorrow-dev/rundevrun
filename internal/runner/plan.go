// Package runner plans and executes lifecycle operations.
package runner

import (
	"context"
	"errors"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

type Runner struct {
	Store      *state.Store
	Executable string
	RetryHooks bool
	Routing    *routing.Config

	ExpectedRuns map[string]string

	SkipStopHooks bool
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
	InstanceID   string            `json:"instance_id"`
	State        string            `json:"state,omitempty"`
	StateUnknown bool              `json:"state_unknown,omitempty"`
	RunID        string            `json:"run_id,omitempty"`
	Status       string            `json:"status"`
	URLs         map[string]string `json:"urls,omitempty"`
	LogPaths     map[string]string `json:"log_paths,omitempty"`
}

func (r *Runner) Plan(ctx context.Context, action string, project *discovery.Project, worktree *discovery.Worktree, service string) (Plan, error) {
	selection := Selection{project, worktree, service}
	id, e := selectionID(selection)
	if e != nil {
		return Plan{}, e
	}
	graph, e := r.PlanGraph(ctx, action, []Selection{selection}, false)
	if e != nil {
		return Plan{}, e
	}
	for _, p := range graph.Instances {
		if p.InstanceID == id {
			return Plan{Action: action, InstanceID: id, State: p.State, RunID: p.RunID, Digest: p.Digest, Revision: graph.Revision, Addresses: p.Addresses, Steps: p.Steps}, nil
		}
	}
	return Plan{}, &Error{"NOT_FOUND", errors.New("selected instance absent from plan")}
}
