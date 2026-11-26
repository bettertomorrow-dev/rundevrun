// Restarts running consumers after their dependency recovers.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/localrouting"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func restartConsumers(ctx context.Context, store *state.Store, source state.Manifest, log io.Writer) map[string]string {
	failures := map[string]string{}
	report := func(id string, err error) {
		if ctx.Err() == nil {
			failures[id] = err.Error()
			fmt.Fprintf(log, "[rundevrun] consumer %s restart failed: %v\n", id, err)
		}
	}
	reg, err := store.Read()
	if err != nil {
		report(source.InstanceID, err)
		return failures
	}
	r := New(store, "")
	parts := strings.Split(source.InstanceID, "/")
	projectID, worktreeID, service := parts[0], parts[1], parts[2]
	project, err := reg.Project(projectID)
	if err != nil {
		report(source.InstanceID, err)
		return failures
	}
	selection := func(worktreeID, service string) Selection {
		for _, w := range project.Worktrees {
			if w.ID == worktreeID {
				return Selection{&discovery.Project{ID: projectID}, &discovery.Worktree{ID: w.ID, Root: w.Root}, service}
			}
		}
		return Selection{}
	}
	plan, err := newGraph(r, reg).down(ctx, []Selection{selection(worktreeID, service)}, true)
	if err != nil {
		report(source.InstanceID, err)
		return failures
	}
	if len(plan.Instances) < 2 {
		return failures
	}
	path, err := routing.DefaultPath()
	if err != nil {
		report(source.InstanceID, err)
		return failures
	}
	cfg, err := localrouting.Load(path)
	if err != nil {
		report(source.InstanceID, err)
		return failures
	}
	r.Routing = &cfg
	r.recoveredDependency = source.InstanceID
	for i := len(plan.Instances) - 1; i >= 0 && ctx.Err() == nil; i-- {
		item := plan.Instances[i]
		if item.InstanceID == source.InstanceID || !liveState(item.State) {
			continue
		}
		blocked := false
		for _, id := range item.Bindings {
			if failures[id] != "" {
				report(item.InstanceID, fmt.Errorf("dependency %s did not restart", id))
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		instance := project.Instances[item.InstanceID]
		r.ExpectedRuns = map[string]string{source.InstanceID: source.RunID, item.InstanceID: item.RunID}
		r.restartBindings = map[string]map[string]string{item.InstanceID: item.Bindings}
		selected := []Selection{selection(instance.WorktreeID, instance.Service)}
		for ctx.Err() == nil {
			result, err := r.RestartGraph(ctx, selected)
			if !errors.Is(err, state.ErrBusy) || len(result.Results) != 0 {
				if err != nil {
					report(item.InstanceID, err)
				}
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return failures
}
