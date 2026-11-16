// Tests monitor monitor behavior.
package monitor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestReadScopesFailuresAndHidesRemovedWorktrees(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "project")
	removed := filepath.Join(base, "removed")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	s := &state.Store{Root: filepath.Join(base, "state")}
	p := state.Project{ID: "p", Name: "project", Root: root, Worktrees: []state.Worktree{{ID: "w", Root: root}, {ID: "gone", Root: removed}}, Instances: map[string]state.Instance{
		"p/w/api":    {ID: "p/w/api", WorktreeID: "w", Service: "api", State: "stopped"},
		"p/gone/api": {ID: "p/gone/api", WorktreeID: "gone", Service: "api", State: "stopped"},
	}}
	if err := s.Update(func(r *state.Registry) error { return r.SetProject(p.ID, p) }); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.RecordLaunch([]state.LaunchResult{
		{Attempt: "one", ProjectID: "p", InstanceID: "p/w/api", StartedAt: now, Error: "BUILD_FAILED: bad build"},
		{Attempt: "one", ProjectID: "p", InstanceID: "p/gone/api", StartedAt: now, Error: "BUILD_FAILED: old worktree"},
	}); err != nil {
		t.Fatal(err)
	}
	view, err := Read(context.Background(), s, root)
	if err != nil {
		t.Fatal(err)
	}
	if view.CurrentProjectID != "p" || !view.PreferErrors || len(view.Services) != 1 || view.Services[0].ID != "p/w/api" {
		t.Fatalf("unexpected snapshot: %+v", view)
	}
	view, err = Read(context.Background(), s, base)
	if err != nil {
		t.Fatal(err)
	}
	if view.CurrentProjectID != "" || !view.PreferErrors {
		t.Fatalf("global scope: %+v", view)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	view, err = Read(context.Background(), s, base)
	if err != nil || view.PreferErrors || len(view.Services) != 0 {
		t.Fatalf("removed worktrees selected an empty errors tab: %+v err=%v", view, err)
	}
}
