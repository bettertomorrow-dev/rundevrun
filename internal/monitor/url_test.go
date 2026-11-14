package monitor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestReadUsesApplicationReportedURL(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &state.Store{Root: filepath.Join(root, "state")}
	const id = "p/w/cards"
	if err := store.Update(func(reg *state.Registry) error {
		return reg.SetProject("p", state.Project{ID: "p", Name: "example", Root: root,
			Worktrees: []state.Worktree{{ID: "w", Root: root}},
			Instances: map[string]state.Instance{id: {ID: id, WorktreeID: "w", Service: "cards", State: "running", RunID: "current"}}})
	}); err != nil {
		t.Fatal(err)
	}
	const base = "http://127.0.0.1:20002"
	if err := store.WriteManifest("current", state.Manifest{RunID: "current", InstanceID: id,
		Addresses: map[string]string{"url": base, "direct_url": base}}); err != nil {
		t.Fatal(err)
	}
	ref, err := procid.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteRun("current", state.RunRecord{Supervisor: ref, Child: ref, State: "ready"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Root, "runs", "current", "application.log")
	for _, tc := range []struct{ name, previous, current, want string }{
		{"twikko", "", "twikko: url=" + base + "/s/example/\n", base + "/s/example/"},
		{"rotated", "twikko: url=" + base + "/s/example/\n", "request https://unrelated.example/\n", base + "/s/example/"},
		{"latest", "twikko: url=" + base + "/s/old/\n", "another-app: url=https://example.test/new/?view=cards\n", "https://example.test/new/?view=cards"},
		{"unsafe", "", "app: url=javascript:alert(1)\napp: url=https://user:secret@example.test/\n", base},
		{"ordinary URL", "", "see https://example.test/docs\n", base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for suffix, content := range map[string]string{".1": tc.previous, "": tc.current} {
				if err := os.WriteFile(path+suffix, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			view, err := Read(context.Background(), store, root)
			if err != nil || len(view.Services) != 1 {
				t.Fatalf("snapshot=%+v err=%v", view, err)
			}
			if svc := view.Services[0]; svc.URL != tc.want || svc.DirectURL != base {
				t.Fatalf("URL=%q direct=%q, want URL=%q direct=%q", svc.URL, svc.DirectURL, tc.want, base)
			}
		})
	}
}

func TestEndpointURLsExposeLocalAndTailnetPorts(t *testing.T) {
	local := state.Endpoint{Scope: "local", Hostname: "cards-w-main-example.localhost", Port: 8080}
	tailnet := state.Endpoint{Scope: "tailnet", Hostname: "cards-w-main-example.dev.example", Port: 443}
	if got := endpointURL(local); got != "http://cards-w-main-example.localhost:8080" {
		t.Fatalf("local URL=%q", got)
	}
	if got := endpointURL(tailnet); got != "https://cards-w-main-example.dev.example:443" {
		t.Fatalf("tailnet URL=%q", got)
	}
}
