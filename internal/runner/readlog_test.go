package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestLogsTailOrder(t *testing.T) {
	dir := t.TempDir()
	store, err := state.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := "p/w/s"
	err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject("p", state.Project{ID: "p", Name: "p", Root: dir, Worktrees: []state.Worktree{{ID: "w", Root: dir}}, Instances: map[string]state.Instance{id: {ID: id, Service: "s", WorktreeID: "w", State: "stopped", RunID: "run"}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.WriteManifest("run", state.Manifest{RunID: "run", InstanceID: id}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "runs", "run", "application.log"), []byte("a\nb\nc\nd\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = New(store, "").Logs(context.Background(), &discovery.Project{ID: "p"}, &discovery.Worktree{ID: "w"}, "s", 2, false, &out)
	if err != nil || out.String() != "c\nd\n" {
		t.Fatalf("got %q: %v", out.String(), err)
	}
}
