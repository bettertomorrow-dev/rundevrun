package runner

import (
	"strings"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
)

func TestSnapshotEnvironmentUsesPublicDependencyURL(t *testing.T) {
	svc := config.Service{Env: map[string]string{"API": "{dependencies.api.url}", "OVERRIDE": "old"}, Share: map[string]any{"build": "printf %s {dependencies.api.url}", "env": map[string]any{"OVERRIDE": "{dependencies.api.url}"}}}
	cmd, env, err := SnapshotEnvironment(svc, map[string]string{"host": "127.0.0.1"}, map[string]map[string]string{"api": {"url": "https://demo.trycloudflare.com/api", "direct_url": "http://127.0.0.1:1234"}})
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
	if !strings.Contains(cmd, "__RDR_") {
		t.Fatalf("command did not use safe substitution: %s", cmd)
	}
}
