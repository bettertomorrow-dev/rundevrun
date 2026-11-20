// Verifies guide selection and onboarding configuration reporting.
package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuides(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	t.Setenv("RDR_HOME", filepath.Join(base, "state"))
	list, code := invoke(t, "guide")
	if code != 0 || len(list.Results) != 2 || list.Results[0].(map[string]any)["guide"] != "onboard" || list.Results[1].(map[string]any)["guide"] != "domain" {
		t.Fatalf("guide list: %+v", list)
	}
	if _, err := os.Stat(os.Getenv("XDG_CONFIG_HOME")); !os.IsNotExist(err) {
		t.Fatalf("listing accessed guide cache: %v", err)
	}
	for _, tc := range []struct {
		args []string
		name string
	}{
		{[]string{"guide", "onboard", "--no-update"}, "onboard"},
		{[]string{"guide", "domain", "--no-update"}, "domain"},
	} {
		e, code := invoke(t, tc.args...)
		if code != 0 {
			t.Fatalf("%v: %d %+v", tc.args, code, e)
		}
		row := e.Results[0].(map[string]any)
		if row["guide"] != tc.name || !filepath.IsAbs(row["guide_dir"].(string)) || row["instructions"] == "" {
			t.Fatalf("%v: %+v", tc.args, row)
		}
		if tc.name == "onboard" && (row["config_state"] != "missing" || row["worktree_root"] != root) {
			t.Fatalf("onboard: %+v", row)
		}
	}
	if e, code := invoke(t, "config", "check", "--json"); code == 0 || !strings.Contains(strings.Join(e.Errors[0].Actions, " "), "onboard") {
		t.Fatalf("missing hint: %+v", e)
	}
	file := filepath.Join(root, "rundevrun.toml")
	if err := os.WriteFile(file, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	e, code := invoke(t, "guide", "onboard", "--no-update")
	if code != 0 {
		t.Fatalf("invalid config: %+v", e)
	}
	row := e.Results[0].(map[string]any)
	if row["config_state"] != "invalid" || row["config_path"] != file || row["config_mode"] != "shared" || row["config_error"] == "" {
		t.Fatalf("invalid config: %+v", row)
	}
	if e, code := invoke(t, "guide", "domain", "--file", "x"); code == 0 {
		t.Fatalf("accepted flag: %+v", e)
	}
}
