// Resolves personal configuration outside Git repositories without changing their files.
package discovery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
)

func ExternalConfigPath(start string) (root, filename string, err error) {
	root, err = canonical(start)
	if err != nil {
		return "", "", err
	}
	root, err = git(root, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", fmt.Errorf("external configuration requires a Git worktree: %w", ErrNotFound)
	}
	root, err = canonical(root)
	if err != nil {
		return "", "", err
	}
	common, err := git(root, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", "", err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	common, err = canonical(common)
	if err != nil {
		return "", "", err
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return "", "", e
		}
		base = filepath.Join(home, ".config")
	}
	filename, err = filepath.Abs(filepath.Join(base, "rundevrun", "projects", id("p_", common), "rundevrun.toml"))
	return root, filename, err
}

func LoadConfig(root string) (*config.Config, error) {
	_, file, err := ExternalConfigPath(root)
	if err == nil {
		if _, err = os.Lstat(file); err == nil {
			return config.LoadForRoot(file, root)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return config.LoadForRoot(filepath.Join(root, "rundevrun.toml"), root)
}
