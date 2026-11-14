package runner

import (
	"path/filepath"
	"testing"
)

// Worktree discovery returns physical paths, including through macOS /var.
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}
