// Verifies external onboarding without runner files in the checkout.
package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/monitor"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestExternalConfigOnboarding(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q", root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	t.Chdir(root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	runtime := filepath.Join(base, "state")
	t.Setenv("RDR_HOME", runtime)
	source := filepath.Join(base, "source.toml")
	data := []byte("version=1\n[project]\nname=\"demo\"\n[services.worker]\ncommand=\"touch executed; sleep 60\"\n")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	e, code := invoke(t, "config", "external", "--json")
	if code != 0 {
		t.Fatalf("external path: %d %+v", code, e)
	}
	file := e.Results[0].(map[string]any)["config_file"].(string)
	for _, args := range [][]string{
		{"config", "external", "--file", source, "--dry-run", "--json"},
		{"config", "external", "--file", source, "--all-services", "--json"},
	} {
		e, code := invoke(t, args...)
		if (code == 0) != !contains(args, "--all-services") {
			t.Fatalf("%v: %d %+v", args, code, e)
		}
		if _, err := os.Stat(filepath.Dir(file)); !os.IsNotExist(err) {
			t.Fatalf("read-only invocation created config directory: %v", err)
		}
	}
	if e, code := invoke(t, "config", "external", "--file", source, "--json"); code != 0 {
		t.Fatalf("import: %d %+v", code, e)
	}
	if e, code := invoke(t, "config", "external", "--file", source, "--json"); code == 0 {
		t.Fatalf("overwrote existing config: %+v", e)
	}
	info, err := os.Stat(file)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config permissions: %v %v", info, err)
	}
	if e, code := invoke(t, "config", "check", "--json"); code != 0 {
		t.Fatalf("check: %d %+v", code, e)
	} else if row := e.Results[0].(map[string]any); row["config_file"] != file || row["config_mode"] != "external" {
		t.Fatalf("config provenance: %+v", row)
	}
	if e, code := invoke(t, "up", "worker", "--dry-run", "--json"); code != 0 {
		t.Fatalf("up plan: %d %+v", code, e)
	}
	if _, err := os.Stat(runtime); !os.IsNotExist(err) {
		t.Fatalf("check or dry-run created runtime: %v", err)
	}
	e, code = invoke(t, "register", "--json")
	if code != 0 {
		t.Fatalf("register: %d %+v", code, e)
	}
	projectID := e.Scope.Project
	for _, command := range []string{"services", "status"} {
		if e, code := invoke(t, command, "--json"); code != 0 || len(e.Results) != 1 {
			t.Fatalf("%s: %d %+v", command, code, e)
		}
	}
	snapshot, err := monitor.Read(context.Background(), &state.Store{Root: runtime}, root)
	if err != nil || snapshot.CurrentProjectID != projectID {
		t.Fatalf("rundevtop project selection: %+v %v", snapshot, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".git" {
		t.Fatalf("onboarding wrote to checkout: %v %v", entries, err)
	}
}
