// Tests runner local alias behavior.
package runner

import (
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestLocalShortAliasRequiresUnambiguousChoices(t *testing.T) {
	cfg := &config.Config{Services: map[string]config.Service{"api": {Protocol: "http"}, "web": {Protocol: "http"}}}
	project := &discovery.Project{ID: "p", Name: "demo", Worktrees: []discovery.Worktree{{ID: "a", Branch: "main", Config: cfg}, {ID: "b", Branch: "master", Config: cfg}}}
	instances := map[string]state.Instance{}
	for _, w := range project.Worktrees {
		for _, s := range []string{"api", "web"} {
			id := "p/" + w.ID + "/" + s
			instances[id] = state.Instance{ID: id, WorktreeID: w.ID, Service: s}
		}
	}
	projects := map[string]state.Project{"p": {ID: "p", Name: "demo", Instances: instances}}
	selections := map[string]Selection{"p/a/api": {Project: project, Worktree: &project.Worktrees[0], Service: "api"}}
	r := &Runner{Routing: &routing.Config{LocalOnly: true, Routing: routing.Settings{Hostname: "{service}-{worktree}-{project}.{domain}", HTTPListen: "127.0.0.1:8080"}, LocalProjects: map[string]routing.LocalProject{}}}
	if err := r.assignLocalAliases(projects, selections, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	for _, i := range projects["p"].Instances {
		if len(i.Endpoints) != 0 {
			t.Fatalf("ambiguous alias created: %+v", i.Endpoints)
		}
	}
	r.Routing.LocalProjects["p"] = routing.LocalProject{PrimaryWorktree: "b"}
	if err := r.assignLocalAliases(projects, selections, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	for _, i := range projects["p"].Instances {
		if len(i.Endpoints) != 0 {
			t.Fatalf("ambiguous service alias created: %+v", i.Endpoints)
		}
	}
	r.Routing.LocalProjects["p"] = routing.LocalProject{PrimaryWorktree: "b", DefaultService: "web"}
	if err := r.assignLocalAliases(projects, selections, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	endpoint := projects["p"].Instances["p/b/web"].Endpoints
	if len(endpoint) != 1 || endpoint[0].Hostname != "demo.localhost" || !endpoint[0].Alias {
		t.Fatalf("wrong selected alias: %+v", endpoint)
	}
}
