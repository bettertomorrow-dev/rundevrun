// Package publication manages explicitly requested public gateways.
package publication

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

type Options struct {
	Mode     string
	Recreate bool
}
type Manager struct {
	Store      *state.Store
	Executable string
}
type Target struct {
	InstanceID string `json:"instance_id"`
	RunID      string `json:"run_id"`
}
type LivePlan struct {
	ProjectID string   `json:"project_id"`
	Root      Target   `json:"root"`
	Routed    []Target `json:"routed"`
	Locks     []string `json:"locks"`
	Revision  uint64   `json:"revision"`
}
type Status struct {
	ID     string   `json:"id,omitempty"`
	Mode   string   `json:"mode,omitempty"`
	State  string   `json:"state"`
	URL    string   `json:"url,omitempty"`
	Root   Target   `json:"root"`
	Routed []Target `json:"routed,omitempty"`
}
type Result struct {
	Status
	Password string `json:"-"`
}

// PlanLive verifies the exact active HTTP root and explicitly routed targets.
// It does not create locks, directories, reservations, listeners or processes.
func (m *Manager) PlanLive(ctx context.Context, selection runner.Selection) (LivePlan, error) {
	if e := ctx.Err(); e != nil {
		return LivePlan{}, e
	}
	if m == nil || m.Store == nil || selection.Project == nil || selection.Worktree == nil || selection.Service == "" {
		return LivePlan{}, errors.New("missing publication selection")
	}
	reg, e := m.Store.Read()
	if e != nil {
		return LivePlan{}, e
	}
	project, e := reg.Project(selection.Project.ID)
	if e != nil {
		return LivePlan{}, e
	}
	known := false
	for _, w := range project.Worktrees {
		if w.ID == selection.Worktree.ID && w.Root == selection.Worktree.Root {
			known = true
			break
		}
	}
	if !known {
		return LivePlan{}, errors.New("worktree identity not registered")
	}
	id := project.ID + "/" + selection.Worktree.ID + "/" + selection.Service
	instance, ok := project.Instances[id]
	if !ok || instance.RunID == "" {
		return LivePlan{}, errors.New("publication root is not running")
	}
	manifest, e := m.Store.ReadManifest(instance.RunID)
	if e != nil {
		return LivePlan{}, e
	}
	if manifest.InstanceID != id || manifest.Launch.Protocol != "http" {
		return LivePlan{}, errors.New("publication root is not an HTTP run")
	}
	if _, e = runner.VerifyProxyTarget(m.Store, project.ID, id, instance.RunID); e != nil {
		return LivePlan{}, fmt.Errorf("root not ready: %w", e)
	}
	out := LivePlan{ProjectID: project.ID, Root: Target{id, instance.RunID}, Routed: []Target{}, Locks: []string{id}, Revision: reg.Revision}
	seen := map[string]bool{id: true}
	for _, route := range manifest.Routes {
		targetID := manifest.EffectiveBindings[route.Dependency]
		parts := strings.Split(targetID, "/")
		if len(parts) != 3 || parts[0] != project.ID || parts[2] != route.Dependency {
			return LivePlan{}, fmt.Errorf("invalid routed binding %q", route.Dependency)
		}
		target, exists := project.Instances[targetID]
		if !exists || target.RunID == "" {
			return LivePlan{}, fmt.Errorf("routed dependency %q is not running", route.Dependency)
		}
		if _, e = runner.VerifyProxyTarget(m.Store, project.ID, targetID, target.RunID); e != nil {
			return LivePlan{}, fmt.Errorf("routed dependency %q not ready: %w", route.Dependency, e)
		}
		if !seen[targetID] {
			seen[targetID] = true
			out.Routed = append(out.Routed, Target{targetID, target.RunID})
			out.Locks = append(out.Locks, targetID)
		}
	}
	sort.Strings(out.Locks)
	sort.Slice(out.Routed, func(i, j int) bool { return out.Routed[i].InstanceID < out.Routed[j].InstanceID })
	return out, nil
}
