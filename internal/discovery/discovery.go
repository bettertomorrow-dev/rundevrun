package discovery

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
)

var ErrAmbiguous = errors.New("ambiguous selector")
var ErrNotFound = errors.New("selector not found")

type Project struct {
	ID, Name, Root, GitCommonDir string
	Worktrees                    []Worktree
}
type Worktree struct {
	ID, Root, Branch string
	Detached         bool
	Config           *config.Config
	ConfigError      error
}

func canonical(p string) (string, error) {
	a, e := filepath.Abs(p)
	if e != nil {
		return "", e
	}
	a, e = filepath.EvalSymlinks(a)
	if e != nil {
		return "", e
	}
	return filepath.Clean(a), nil
}
func id(prefix, s string) string {
	h := sha256.Sum256([]byte(s))
	return prefix + hex.EncodeToString(h[:])
}
func git(dir string, args ...string) (string, error) {
	c := exec.Command("git", append([]string{"-c", "core.fsmonitor=false"}, args...)...)
	c.Dir = dir
	c.Env = cleanGitEnv(os.Environ())
	b, e := c.Output()
	if e != nil {
		return "", e
	}
	return strings.TrimSpace(string(b)), nil
}
func findConfig(start string) (string, error) {
	p := start
	for {
		f := filepath.Join(p, "dev.toml")
		if _, e := os.Stat(f); e == nil {
			return f, nil
		} else if !errors.Is(e, os.ErrNotExist) {
			return "", e
		}
		n := filepath.Dir(p)
		if n == p {
			return "", os.ErrNotExist
		}
		p = n
	}
}
func Discover(start string) (*Project, error) {
	root, e := canonical(start)
	if e != nil {
		return nil, e
	}
	cf, e := findConfig(root)
	if e != nil {
		return nil, e
	}
	root = filepath.Dir(cf)
	cfg, e := config.Load(cf)
	if e != nil {
		return nil, e
	}
	p := &Project{Name: cfg.Project.Name, Root: root}
	common, ge := git(root, "rev-parse", "--git-common-dir")
	if ge == nil {
		if !filepath.IsAbs(common) {
			common = filepath.Join(root, common)
		}
		common, e = canonical(common)
		if e != nil {
			return nil, e
		}
		p.GitCommonDir = common
		p.ID = id("p_", common)
		out, er := git(root, "worktree", "list", "--porcelain", "-z")
		if er != nil {
			return nil, er
		}
		for _, rec := range strings.Split(out, "\x00\x00") {
			if rec == "" {
				continue
			}
			lines := strings.Split(rec, "\x00")
			var wt string
			branch := ""
			det := false
			for _, line := range lines {
				switch {
				case strings.HasPrefix(line, "worktree "):
					wt = strings.TrimPrefix(line, "worktree ")
				case strings.HasPrefix(line, "branch refs/heads/"):
					branch = strings.TrimPrefix(line, "branch refs/heads/")
				case line == "detached":
					det = true
				}
			}
			if wt == "" {
				continue
			}
			if !filepath.IsAbs(wt) {
				wt = filepath.Join(root, wt)
			}
			wt, e = canonical(wt)
			if e != nil {
				continue
			}
			w := Worktree{ID: id("w_", wt), Root: wt, Branch: branch, Detached: det}
			f := filepath.Join(wt, "dev.toml")
			if b, er := config.Load(f); er == nil {
				if b.Project.Name != cfg.Project.Name {
					w.ConfigError = &config.Error{Code: "CONFIG_INVALID", Field: "project.name", Err: fmt.Errorf("%q differs from %q", b.Project.Name, cfg.Project.Name)}
				} else {
					w.Config = b
				}
			} else {
				w.ConfigError = er
			}
			p.Worktrees = append(p.Worktrees, w)
		}
	} else {
		p.GitCommonDir = ""
		p.ID = id("p_", root)
		p.Worktrees = []Worktree{{ID: id("w_", root), Root: root, Config: cfg}}
	}
	return p, nil
}
func Resolve(p *Project, selector string) (*Worktree, error) {
	w, err := ResolveDescriptor(p, selector)
	if err != nil {
		return nil, err
	}
	return selected(w)
}

// ResolveDescriptor resolves identity without requiring usable configuration.
// Stop and logs can use registered descriptors after configuration is removed.
func ResolveDescriptor(p *Project, selector string) (*Worktree, error) {
	for i := range p.Worktrees {
		if p.Worktrees[i].ID == selector {
			return &p.Worktrees[i], nil
		}
	}
	original := selector
	if filepath.IsAbs(selector) {
		if c, e := canonical(selector); e == nil {
			for i := range p.Worktrees {
				if p.Worktrees[i].Root == c {
					return &p.Worktrees[i], nil
				}
			}
		}
	} else if a, e := filepath.Abs(selector); e == nil {
		if c, e := canonical(a); e == nil {
			for i := range p.Worktrees {
				if p.Worktrees[i].Root == c {
					return &p.Worktrees[i], nil
				}
			}
		}
	}
	var matches []Worktree
	for i := range p.Worktrees {
		w := p.Worktrees[i]
		if original == filepath.Base(w.Root) || original == w.Branch {
			matches = append(matches, w)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("worktree %q: %w", original, ErrNotFound)
	}
	if len(matches) > 1 {
		var names []string
		for _, w := range matches {
			names = append(names, w.ID+" "+w.Root)
		}
		return nil, fmt.Errorf("ambiguous worktree %q: %w: %s", original, ErrAmbiguous, strings.Join(names, "; "))
	}
	return &matches[0], nil
}
func selected(w *Worktree) (*Worktree, error) {
	if w.Config == nil {
		return nil, fmt.Errorf("worktree %q has invalid or missing dev.toml: %w", w.Root, errors.Join(w.ConfigError))
	}
	return w, nil
}

func cleanGitEnv(env []string) []string {
	out := make([]string, 0, len(env)+5)
	for _, v := range env {
		if strings.HasPrefix(v, "GIT_") {
			continue
		}
		out = append(out, v)
	}
	return append(out, "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
}
