// Verifies that interrupted cleanup resumes without touching unrelated files.
package statemigration

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestResumeLegacyCleanup(t *testing.T) {
	base := t.TempDir()
	t.Setenv("RDR_HOME", "")
	t.Setenv("XDG_STATE_HOME", base)
	current := &state.Store{Root: filepath.Join(base, "current")}
	legacy := filepath.Join(base, "rundevrun")
	if err := os.Mkdir(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(legacy, "registry.json")
	if err := os.WriteFile(registry, []byte("corrupt registry"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(context.Background(), current, true, io.Discard); !errors.Is(err, state.ErrCorrupt) {
		t.Fatalf("expected a corrupt registry error, got %v", err)
	}
	if _, err := os.Stat(registry); err != nil {
		t.Fatalf("registry removed before migration: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacy, cleanupMarker), nil, 0600); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(base, "keep")
	if err := os.WriteFile(keep, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(keep, filepath.Join(legacy, "link")); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(context.Background(), current, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(base); err != nil || len(entries) != 1 || entries[0].Name() != "keep" {
		t.Fatalf("legacy state remains or unrelated files were removed: %v %v", entries, err)
	}
}
