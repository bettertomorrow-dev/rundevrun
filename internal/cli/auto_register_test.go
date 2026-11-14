package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUpDryRunPlansBeforeRegistration(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "project")
	stateDir := filepath.Join(base, "state")
	writeProject(t, project)
	t.Setenv("RDR_HOME", stateDir)
	t.Chdir(project)
	e, code := invoke(t, "up", "worker", "--dry-run", "--json")
	if code != 0 || e.Plan == nil {
		t.Fatalf("unregistered dry run: %d %+v", code, e)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run wrote state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, "executed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run executed project code: %v", err)
	}
}
