package discovery

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	if b, e := c.CombinedOutput(); e != nil {
		t.Fatalf("git %v: %s %v", args, b, e)
	}
}
func TestDiscoverDetachedAndAmbiguity(t *testing.T) {
	root := t.TempDir()
	run(t, root, "init", "-q")
	run(t, root, "-c", "user.email=a@b", "-c", "user.name=a", "commit", "--allow-empty", "-m", "init")
	os.WriteFile(filepath.Join(root, "dev.toml"), []byte("version=1\n[project]\nname=\"demo\"\n[services.api]\ncommand=\"true\"\n"), 0600)
	run(t, root, "worktree", "add", "--detach", filepath.Join(filepath.Dir(root), "detached"))
	p, e := Discover(root)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Worktrees) != 2 {
		t.Fatalf("worktrees: %#v", p.Worktrees)
	}
	found := false
	for _, w := range p.Worktrees {
		if w.Detached {
			found = true
			if w.Config != nil {
				t.Fatal("unexpectedly substituted config into detached worktree")
			}
			if _, e = Resolve(p, w.ID); e == nil {
				t.Fatal("resolved worktree without config")
			}
		}
	}
	if !found {
		t.Fatal("detached worktree not recognized")
	}
}
func TestDiscoverThroughSymlink(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "dev.toml"), []byte("version=1\n[project]\nname=\"demo\"\n[services.api]\ncommand=\"true\"\n"), 0600)
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(d, link)
	p, e := Discover(link)
	if e != nil {
		t.Fatal(e)
	}
	if p.Root != d {
		t.Fatalf("root %s != %s", p.Root, d)
	}
}

func TestResolveSymlinkAndRealAmbiguity(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	os.Mkdir(root, 0755)
	run(t, root, "init", "-q")
	run(t, root, "-c", "user.email=a@b", "-c", "user.name=a", "commit", "--allow-empty", "-m", "init")
	os.WriteFile(filepath.Join(root, "dev.toml"), []byte("version=1\n[project]\nname=\"demo\"\n[services.api]\ncommand=\"true\"\n"), 0600)
	wt1 := filepath.Join(parent, "one", "same")
	wt2 := filepath.Join(parent, "two", "same")
	os.MkdirAll(filepath.Dir(wt1), 0755)
	os.MkdirAll(filepath.Dir(wt2), 0755)
	run(t, root, "worktree", "add", "-b", "one", "-q", wt1)
	run(t, root, "worktree", "add", "-b", "two", "-q", wt2)
	for _, p := range []string{wt1, wt2} {
		os.WriteFile(filepath.Join(p, "dev.toml"), []byte("version=1\n[project]\nname=\"demo\"\n[services.api]\ncommand=\"true\"\n"), 0600)
	}
	p, e := Discover(root)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Resolve(p, "same"); !errors.Is(e, ErrAmbiguous) {
		t.Fatalf("wanted ambiguity, got %v", e)
	}
	link := filepath.Join(parent, "link")
	os.Symlink(wt1, link)
	w, e := Resolve(p, link)
	if e != nil || w.Root != wt1 {
		t.Fatalf("symlink resolution: %#v %v", w, e)
	}
}
func TestIndependentClonesAndGitEnvironment(t *testing.T) {
	parent := t.TempDir()
	roots := []string{filepath.Join(parent, "a"), filepath.Join(parent, "b")}
	for _, r := range roots {
		os.Mkdir(r, 0755)
		run(t, r, "init", "-q")
		run(t, r, "-c", "user.email=a@b", "-c", "user.name=a", "commit", "--allow-empty", "-m", "init")
		os.WriteFile(filepath.Join(r, "dev.toml"), []byte("version=1\n[project]\nname=\"demo\"\n[services.api]\ncommand=\"true\"\n"), 0600)
	}
	t.Setenv("GIT_DIR", "/definitely/not/the/repository")
	t.Setenv("GIT_WORK_TREE", "/definitely/not/the/worktree")
	t.Setenv("GIT_COMMON_DIR", "/definitely/not/common")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.fsmonitor")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	a, e := Discover(roots[0])
	if e != nil {
		t.Fatal(e)
	}
	b, e := Discover(roots[1])
	if e != nil {
		t.Fatal(e)
	}
	if a.ID == b.ID {
		t.Fatal("independent clones merged")
	}
	env := cleanGitEnv([]string{"PATH=/bin", "GIT_DIR=/evil", "GIT_WORK_TREE=/evil", "GIT_COMMON_DIR=/evil", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=true"})
	for _, v := range env {
		if strings.HasPrefix(v, "GIT_DIR=") || strings.HasPrefix(v, "GIT_WORK_TREE=") || strings.HasPrefix(v, "GIT_COMMON_DIR=") || strings.HasPrefix(v, "GIT_CONFIG_COUNT=") {
			t.Fatalf("unsafe environment survived: %s", v)
		}
	}
}

func TestFindConfigPropagatesNonNotFound(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if e := os.WriteFile(f, nil, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := findConfig(f); e == nil {
		t.Fatal("expected ENOTDIR")
	}
}
