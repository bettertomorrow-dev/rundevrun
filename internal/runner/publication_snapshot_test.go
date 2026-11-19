// Tests runner publication snapshot behavior.
package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
)

func TestSnapshotEnvironmentUsesPublicDependencyURL(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("FILE_VALUE=from-file\nOVERRIDE=from-file\nAPI=from-file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FILE_VALUE", "inherited")
	svc := config.Service{Env: map[string]string{"API": "{dependencies.api.url}", "OVERRIDE": "old"}, Share: map[string]any{"build": "printf %s {dependencies.api.url}", "env": map[string]any{"OVERRIDE": "{dependencies.api.url}"}}}
	cmd, env, err := SnapshotEnvironment(svc, map[string]string{"host": "127.0.0.1", "worktree": root}, map[string]map[string]string{"api": {"url": "https://demo.trycloudflare.com/api", "direct_url": "http://127.0.0.1:1234"}}, config.Project{EnvFiles: []string{".env"}})
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]string{}
	for _, item := range env {
		k, v, ok := strings.Cut(item, "=")
		if ok {
			entries[k] = v
		}
	}
	for _, key := range []string{"API", "OVERRIDE", "RDR_DEP_API_URL"} {
		if entries[key] != "https://demo.trycloudflare.com/api" {
			t.Fatalf("%s=%q", key, entries[key])
		}
	}
	if entries["RDR_DEP_API_DIRECT_URL"] != "http://127.0.0.1:1234" {
		t.Fatal(entries)
	}
	if entries["FILE_VALUE"] != "from-file" {
		t.Fatal("snapshot build did not receive dotenv values")
	}
	if !strings.Contains(cmd, "__RDR_") {
		t.Fatalf("command did not use safe substitution: %s", cmd)
	}
}
