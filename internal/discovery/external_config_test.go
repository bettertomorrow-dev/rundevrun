// Verifies external configuration selection and worktree confinement.
package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExternalConfigAcrossWorktrees(t *testing.T) {
	base := canonicalTempDir(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	root := filepath.Join(base, "repo")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	run(t, root, "init", "-q")
	run(t, root, "-c", "user.email=a@b", "-c", "user.name=a", "commit", "--allow-empty", "-m", "init")
	other := filepath.Join(base, "other")
	run(t, root, "worktree", "add", "--detach", other)
	for _, wt := range []string{root, other} {
		if err := os.Mkdir(filepath.Join(wt, "app"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	_, file, err := ExternalConfigPath(filepath.Join(other, "app"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte("version=1\n[project]\nname=\"demo\"\n[services.api]\ncommand=\"true\"\ncwd=\"app\"\n")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	p, err := Discover(filepath.Join(other, "app"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Worktrees) != 2 || filepath.Base(filepath.Dir(file)) != p.ID || filepath.Base(file) != "rundevrun.toml" {
		t.Fatalf("project identity: %+v, config %s", p, file)
	}
	for _, w := range p.Worktrees {
		if w.Config == nil || w.Config.Filename != file || w.Config.Root != w.Root {
			t.Fatalf("external worktree: %+v", w)
		}
		cfg, err := LoadConfig(w.Root)
		if err != nil || cfg.Digest != w.Config.Digest {
			t.Fatalf("reload: %+v, %v", cfg, err)
		}
	}
	clone := filepath.Join(base, "clone")
	run(t, base, "clone", "-q", root, clone)
	if _, err := Discover(clone); !os.IsNotExist(err) {
		t.Fatalf("separate clone inherited config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte("invalid TOML ["), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(root); err != nil {
		t.Fatalf("shared config overrode external binding: %v", err)
	}
	if err := os.Remove(filepath.Join(other, "app")); err != nil {
		t.Fatal(err)
	}
	p, err = Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(p, other); err == nil {
		t.Fatal("missing cwd accepted in another worktree")
	}
	if err := os.Symlink(filepath.Dir(file), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("version=1\n[project]\nname=\"demo\"\n[services.api]\ncommand=\"true\"\ncwd=\"escape\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(root); err == nil {
		t.Fatal("cwd escaped worktree through symlink")
	}
	if err := os.WriteFile(file, []byte("invalid TOML ["), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(root); err == nil {
		t.Fatal("invalid external config silently fell back")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "rundevrun.toml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if p, err := Discover(root); err != nil || p.Worktrees[0].Config.Filename != filepath.Join(root, "rundevrun.toml") {
		t.Fatalf("return to shared: %+v %v", p, err)
	}
}
