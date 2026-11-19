// Verifies dotenv inheritance in shared and external worktree service lifecycles.
package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeEnvironmentFiles(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "rundevrun")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	for _, mode := range []string{"shared", "external"} {
		t.Run(mode, func(t *testing.T) {
			base := t.TempDir()
			main := filepath.Join(base, "main")
			other := filepath.Join(base, "worktree")
			if err := os.Mkdir(main, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
			t.Setenv("RDR_HOME", filepath.Join(base, "runtime"))
			t.Setenv("APP_TEST_SHARED", "inherited")
			git := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = main
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
			}
			write := func(root, name, data string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			git("init", "-q")
			git("-c", "user.email=a@b", "-c", "user.name=a", "commit", "--allow-empty", "-qm", "initial")
			cfg := `version = 1
[project]
name = "demo"
env_files = [".env", ".env.local"]
[services.worker]
command = "exec sleep 60"
build = 'printf "%s|%s|%s" "$APP_TEST_SHARED" "$DATABASE_URL" "$EXPLICIT" > built'
env = { EXPLICIT = "service" }
hooks = { before_start = 'printf "%s|%s|%s" "$APP_TEST_SHARED" "$DATABASE_URL" "$EXPLICIT" > before', after_stop = 'printf "%s|%s|%s" "$APP_TEST_SHARED" "$DATABASE_URL" "$EXPLICIT" > stopped' }
`
			t.Chdir(main)
			if mode == "shared" {
				write(main, "rundevrun.toml", cfg)
				git("add", "rundevrun.toml")
				git("-c", "user.email=a@b", "-c", "user.name=a", "commit", "-qm", "runner config")
			} else {
				write(base, "source.toml", cfg)
				if e, code := invoke(t, "config", "external", "--file", filepath.Join(base, "source.toml"), "--json"); code != 0 {
					t.Fatalf("external config: %d %+v", code, e)
				}
			}
			git("worktree", "add", "--detach", other)
			write(main, ".env", "APP_TEST_SHARED=shared-secret\nDATABASE_URL=shared-database\nEXPLICIT=file\n")
			write(main, ".env.local", "DATABASE_URL=shared-local-database\n")
			t.Chdir(other)
			if e, code := invoke(t, "config", "check", "--json"); code != 0 {
				t.Fatalf("fresh worktree config: %d %+v", code, e)
			}
			write(other, ".env", "DATABASE_URL=worktree-database\n")
			for _, args := range [][]string{{"up", "worker", "--dry-run", "--json"}, {"register", ".", "--json"}} {
				e, code := invoke(t, args...)
				out, _ := json.Marshal(e)
				if code != 0 || strings.Contains(string(out), "shared-secret") || strings.Contains(string(out), "worktree-database") {
					t.Fatalf("%v: %d %s", args, code, out)
				}
			}
			t.Cleanup(func() { _, _ = invokeBinary(t, binary, "down", "worker") })
			if e, code := invokeBinary(t, binary, "up", "worker"); code != 0 {
				t.Fatalf("up: %d %+v", code, e)
			}
			write(other, ".env", "DATABASE_URL=changed-after-start\n")
			if e, code := invokeBinary(t, binary, "down", "worker"); code != 0 {
				t.Fatalf("down: %d %+v", code, e)
			}
			for _, file := range []string{"built", "before", "stopped"} {
				data, err := os.ReadFile(filepath.Join(other, file))
				if err != nil || string(data) != "shared-secret|worktree-database|service" {
					t.Fatalf("%s: %q %v", file, data, err)
				}
			}
			write(other, ".env", "KEY=\"private-secret")
			for _, args := range [][]string{{"config", "check", "--json"}, {"up", "worker", "--dry-run", "--json"}} {
				e, code := invoke(t, args...)
				out, _ := json.Marshal(e)
				if code == 0 || strings.Contains(string(out), "private-secret") {
					t.Fatalf("malformed dotenv %v: %d %s", args, code, out)
				}
			}
		})
	}
}
