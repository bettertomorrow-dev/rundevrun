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

func ActiveConfig(start string) (root, file, mode string, err error) {
	root, external, err := ExternalConfigPath(start)
	if err != nil {
		return "", "", "", err
	}
	if _, err := os.Lstat(external); err == nil {
		return root, external, "external", nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", "", err
	}
	shared := filepath.Join(root, "rundevrun.toml")
	if _, err := os.Lstat(shared); err == nil {
		return root, shared, "shared", nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", "", err
	}
	return root, "", "", nil
}

func LoadConfig(root string) (*config.Config, error) {
	_, file, _, err := ActiveConfig(root)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if file == "" {
		file = filepath.Join(root, "rundevrun.toml")
	}
	return config.LoadForRoot(file, root)
}
