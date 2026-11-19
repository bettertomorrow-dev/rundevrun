// Loads opted-in dotenv files from the main checkout and selected worktree.
package discovery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/joho/godotenv"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func Environment(root string, project config.Project) (map[string]string, error) {
	env := map[string]string{}
	if len(project.EnvFiles) == 0 {
		return env, nil
	}
	roots := []string{root}
	if top, err := git(root, "rev-parse", "--show-toplevel"); err == nil {
		out, err := git(root, "worktree", "list", "--porcelain", "-z")
		if err != nil {
			return nil, fmt.Errorf("project.env_files: discover main checkout: %w", err)
		}
		first, _, _ := strings.Cut(out, "\x00\x00")
		fields := strings.Split(first, "\x00")
		bare := false
		for _, field := range fields {
			bare = bare || field == "bare"
		}
		if len(fields) > 0 && strings.HasPrefix(fields[0], "worktree ") && !bare {
			rel, err := filepath.Rel(top, root)
			if err != nil {
				return nil, fmt.Errorf("project.env_files: resolve checkout: %w", err)
			}
			main := filepath.Join(strings.TrimPrefix(fields[0], "worktree "), rel)
			if main != root {
				roots = []string{main, root}
			}
		}
	}
	for _, dir := range roots {
		for _, path := range project.EnvFiles {
			if !filepath.IsLocal(path) || filepath.Clean(path) == "." {
				return nil, fmt.Errorf("project.env_files: paths must name files relative to the checkout root")
			}
			filename := filepath.Join(dir, path)
			info, err := os.Stat(filename)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("project.env_files: %w", err)
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("project.env_files: %s must be a regular file", filename)
			}
			data, err := os.ReadFile(filename)
			if err != nil {
				return nil, fmt.Errorf("project.env_files: %w", err)
			}
			parsed, err := godotenv.UnmarshalBytes(data)
			if err != nil {
				return nil, fmt.Errorf("project.env_files: invalid dotenv syntax in %s", filename)
			}
			for key, value := range parsed {
				if !envName.MatchString(key) || strings.HasPrefix(key, "RDR_") || strings.HasPrefix(key, "__RDR_") || strings.ContainsRune(value, 0) {
					return nil, fmt.Errorf("project.env_files: invalid or reserved environment entry in %s", filename)
				}
				env[key] = value
			}
		}
	}
	return env, nil
}
