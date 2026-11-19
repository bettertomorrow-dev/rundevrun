// Tests state discovery without initializing runtime storage or requiring a project.
package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigState(t *testing.T) {
	base := t.TempDir()
	t.Chdir(base)
	t.Setenv("RDR_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "legacy"))
	legacy := filepath.Join(base, "legacy", "rundevrun")
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "registry.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
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
	if e, code := invoke(t, "status", "--json"); code == 0 || len(e.Errors) != 1 {
		t.Fatalf("legacy state ignored: %d %+v", code, e)
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
