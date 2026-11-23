// Tests state discovery without initializing runtime storage or requiring a project.
package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestConfigState(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	t.Setenv("RDR_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "legacy"))
	want := filepath.Join(base, "config", "rundevrun", "state")
	e, code := invoke(t, "config", "state", "--json")
	if code != 0 || e.Command != "config state" || len(e.Results) != 1 || e.Results[0].(map[string]any)["state_dir"] != want {
		t.Fatalf("config state: %d %+v", code, e)
	}
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), []string{"config", "state"}, &out, &errOut); code != 0 || out.String() != want+"\n" || errOut.Len() != 0 {
		t.Fatalf("text: %d %q %q", code, out.String(), errOut.String())
	}
	for _, args := range [][]string{
		{"config", "state", "extra", "--json"},
		{"config", "state", "--project", "demo", "--json"},
		{"config", "state", "--worktree", ".", "--json"},
		{"config", "state", "--dry-run", "--json"},
	} {
		if e, code := invoke(t, args...); code != 2 || len(e.Errors) != 1 || e.Errors[0].Code != "USAGE" {
			t.Fatalf("args %v: %d %+v", args, code, e)
		}
	}
	if _, err := os.Stat(want); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created runtime directory: %v", err)
	}
	t.Setenv("RDR_HOME", filepath.Join(base, "override"))
	e, code = invoke(t, "config", "state", "--json")
	if code != 0 || e.Results[0].(map[string]any)["state_dir"] != filepath.Join(base, "override") {
		t.Fatalf("override: %d %+v", code, e)
	}
}

func TestProxyStopMigratesLegacyState(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	t.Setenv("RDR_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "legacy state"))
	legacy := filepath.Join(base, "legacy state", "rundevrun")
	current := filepath.Join(base, "config", "rundevrun", "state")
	for _, fixture := range []struct {
		root, name, id string
	}{{legacy, "old", "shared"}, {legacy, "legacy only", "legacy"}, {current, "current", "shared"}} {
		store := &state.Store{Root: fixture.root}
		if err := store.Update(func(reg *state.Registry) error {
			return reg.SetProject(fixture.id, state.Project{ID: fixture.id, Name: fixture.name, Root: base})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if e, code := invoke(t, "proxy", "stop", "--dry-run", "--json"); code == 0 ||
		len(e.Errors) != 1 || !strings.Contains(e.Errors[0].Message, "rundevrun config migrate-state") {
		t.Fatalf("dry-run should report pending migration: %d %+v", code, e)
	}
	e, code := invoke(t, "proxy", "stop", "--json")
	if code != 0 || e.Proxy == nil || e.Proxy.State != "stopped" {
		t.Fatalf("proxy stop: %d %+v", code, e)
	}
	if entries, err := os.ReadDir(filepath.Dir(legacy)); err != nil || len(entries) != 0 {
		t.Fatalf("legacy state or backup remains: %v %v", entries, err)
	}
	reg, err := (&state.Store{Root: current}).Read()
	if err != nil {
		t.Fatal(err)
	}
	for id, name := range map[string]string{"shared": "current", "legacy": "legacy only"} {
		if project, err := reg.Project(id); err != nil || project.Name != name {
			t.Fatalf("project %s: %+v %v", id, project, err)
		}
	}
	if e, code := invoke(t, "config", "migrate-state", "--json"); code != 0 {
		t.Fatalf("repeated migration: %d %+v", code, e)
	}
}
