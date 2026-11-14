package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestRoutePreviewListsOwnedChangesAndRejectsStaleRegistry(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "project")
	if err := os.Mkdir(projectDir, 0700); err != nil {
		t.Fatal(err)
	}
	content := "version=1\n[project]\nname=\"demo\"\n[services.web]\ncommand=\"echo ready\"\nport=\"auto\"\n"
	if err := os.WriteFile(filepath.Join(projectDir, "dev.toml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := discovery.Discover(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	w := p.Worktrees[0]
	id := p.ID + "/" + w.ID + "/web"
	store, err := state.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: map[string]state.Instance{id: {ID: id, WorktreeID: w.ID, Service: "web", State: "stopped"}}})
	}); err != nil {
		t.Fatal(err)
	}
	cfg := routing.Config{LocalOnly: true, Routing: routing.Settings{Enabled: true, Domain: "localhost", Hostname: "{service}-{worktree}-{project}.{domain}", HTTPListen: "127.0.0.1:8080"}}
	before, _ := store.Read()
	plan, err := PlanLocalRoutes(context.Background(), store, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Revision != before.Revision || len(plan.Changes) != 1 || plan.Changes[0].Action != "add" || plan.Changes[0].InstanceID != id || plan.Changes[0].Hostname != "web-project-demo.localhost" || plan.Changes[0].Port != 8080 || plan.Changes[0].Scope != "local" {
		t.Fatalf("preview=%+v", plan)
	}
	after, _ := store.Read()
	if after.Revision != before.Revision {
		t.Fatal("preview wrote registry")
	}
	if err = store.Update(func(*state.Registry) error { return nil }); err != nil {
		t.Fatal(err)
	}
	err = CommitLocalRoutes(context.Background(), store, plan)
	var re *Error
	if !errors.As(err, &re) || re.Code != "STATE_CONFLICT" {
		t.Fatalf("stale preview committed: %v", err)
	}
}
