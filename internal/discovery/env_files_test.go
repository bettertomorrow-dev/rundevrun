// Tests shared dotenv precedence, opt-in behavior, and safe failures.
package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
)

func TestEnvironmentFiles(t *testing.T) {
	main := canonicalTempDir(t)
	other := filepath.Join(canonicalTempDir(t), "detached worktree")
	write := func(root, path, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(main, ".env", "SHARED=secret\nORDER=main\nexport QUOTED='with # spaces'\nMULTILINE=\"one\\ntwo\"\nJSON='{\"key\":\"value\"}'\n")
	write(main, ".env.local", "ORDER=main-local\nLOCAL=main-local\n")
	project := config.Project{EnvFiles: []string{".env", ".env.local", ".env.missing"}}
	env, err := Environment(main, project)
	if err != nil || env["ORDER"] != "main-local" || env["MULTILINE"] != "one\ntwo" || env["QUOTED"] != "with # spaces" || env["JSON"] != `{"key":"value"}` {
		t.Fatalf("non-Git dotenv: %v %v", env, err)
	}
	run(t, main, "init", "-q")
	run(t, main, "-c", "user.email=a@b", "-c", "user.name=a", "commit", "--allow-empty", "-qm", "init")
	run(t, main, "worktree", "add", "--detach", other)
	env, err = Environment(other, project)
	if err != nil || env["SHARED"] != "secret" || env["ORDER"] != "main-local" {
		t.Fatalf("fresh worktree: %v %v", env, err)
	}
	write(other, ".env", "ORDER=worktree\nSHARED=\n")
	env, err = Environment(other, project)
	if err != nil || env["ORDER"] != "worktree" || env["SHARED"] != "" || env["LOCAL"] != "main-local" {
		t.Fatalf("worktree overrides all shared files: %v %v", env, err)
	}
	write(other, ".env.local", "ORDER=worktree-local\n")
	env, err = Environment(other, project)
	if err != nil || env["ORDER"] != "worktree-local" {
		t.Fatalf("worktree file order: %v %v", env, err)
	}
	mainEnv, err := Environment(main, project)
	if err != nil || mainEnv["ORDER"] != "main-local" || mainEnv["SHARED"] != "secret" {
		t.Fatalf("main checkout changed: %v %v", mainEnv, err)
	}
	if env, err = Environment(other, config.Project{}); err != nil || len(env) != 0 {
		t.Fatalf("files loaded without opt-in: %v %v", env, err)
	}
	before := os.Getenv("SHARED")
	for _, data := range []string{"KEY=\"private-secret", "RDR_PORT=private-secret\n", "__RDR_COMMAND=private-secret\n", "BAD.KEY=private-secret\n", "KEY=private-secret\x00\n"} {
		write(other, ".env", data)
		if _, err := Environment(other, project); err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatalf("unsafe error: %v", err)
		}
	}
	if os.Getenv("SHARED") != before {
		t.Fatal("dotenv changed rundevrun's own environment")
	}
	if err := os.Remove(filepath.Join(other, ".env")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(other, ".env"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Environment(other, project); err == nil {
		t.Fatal("directory accepted as dotenv file")
	}
	if _, err := Environment(main, config.Project{EnvFiles: []string{"../.env"}}); err == nil {
		t.Fatal("escaping path accepted")
	}
	if got, err := Environment(main, config.Project{EnvFiles: []string{".env.missing"}}); err != nil || len(got) != 0 {
		t.Fatal("missing file must be skipped")
	}
}
