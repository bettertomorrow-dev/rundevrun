// Imports personal project configuration into the user config directory.
package cli

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
)

type externalConfigInfo struct {
	ConfigFile   string `json:"config_file"`
	WorktreeRoot string `json:"worktree_root"`
}

func (i externalConfigInfo) String() string {
	return fmt.Sprintf("External config  %s\nWorktree  %s", singleLine(i.ConfigFile), singleLine(i.WorktreeRoot))
}

func externalConfigCommand(args []string, opts options, e *envelope) {
	if !validArity(args, 0, 1, e) {
		return
	}
	if opts.project != "" || opts.worktree != "" {
		e.fail("USAGE", "config external takes a repository path, not project/worktree selectors")
		return
	}
	if opts.dryRun && opts.file == "" {
		e.fail("USAGE", "--dry-run requires --file for config external")
		return
	}
	start := "."
	if len(args) == 1 {
		start = args[0]
	}
	root, destination, err := discovery.ExternalConfigPath(start)
	if err != nil {
		failError(e, err)
		return
	}
	info := externalConfigInfo{ConfigFile: destination, WorktreeRoot: root}
	if opts.file != "" {
		cfg, err := config.LoadForRoot(opts.file, root)
		if err != nil {
			failError(e, err)
			return
		}
		if _, err := os.Lstat(destination); err == nil {
			e.fail("STATE_CONFLICT", "external config already exists: "+destination, "edit that file directly; move it aside to return to Shared mode")
			return
		} else if !os.IsNotExist(err) {
			failError(e, err)
			return
		}
		if opts.dryRun {
			e.Plan = info
			return
		}
		data, err := os.ReadFile(opts.file)
		if err != nil {
			failError(e, err)
			return
		}
		if fmt.Sprintf("%x", sha256.Sum256(data)) != cfg.Digest {
			e.fail("STATE_CONFLICT", "source configuration changed during import")
			return
		}
		if err := importExternalConfig(destination, data); err != nil {
			failError(e, err)
			return
		}
	}
	e.Results = append(e.Results, info)
}

func importExternalConfig(destination string, data []byte) (err error) {
	if err = os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(destination), ".rundevrun-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Link(f.Name(), destination)
}
