// Retrieves guides and reports the active project configuration.
package cli

import (
	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/guides"
)

type guideResult struct {
	guides.Guide
	WorktreeRoot string `json:"worktree_root,omitempty"`
	ConfigState  string `json:"config_state,omitempty"`
	ConfigMode   string `json:"config_mode,omitempty"`
	ConfigPath   string `json:"config_path,omitempty"`
	ConfigError  string `json:"config_error,omitempty"`
}

func guideCommand(args []string, opts options, e *envelope) {
	if opts.project != "" || opts.worktree != "" || opts.toWorktree != "" || opts.file != "" || opts.mode != "" || opts.all || opts.allServices || opts.cascade || opts.retryHooks || opts.noProxy || opts.recreate || opts.dryRun || opts.follow || opts.tailSet {
		e.fail("USAGE", "guide commands accept only --json and --no-update")
		return
	}
	if !validArity(args, 1, 2, e) {
		return
	}
	if len(args) == 1 {
		for _, name := range guides.Names() {
			e.Results = append(e.Results, map[string]string{"guide": name})
		}
		return
	}
	name := args[1]
	item := guideResult{}
	if name == "onboard" {
		root, path, mode, err := discovery.ActiveConfig(".")
		if err != nil {
			failError(e, err)
			return
		}
		item.WorktreeRoot, item.ConfigState = root, "missing"
		if path != "" {
			item.ConfigPath, item.ConfigMode, item.ConfigState = path, mode, "present"
			if _, err := config.LoadForRoot(path, root); err != nil {
				item.ConfigState, item.ConfigError = "invalid", err.Error()
			}
		}
	} else if name != "domain" {
		e.fail("USAGE", "unknown guide: "+name, "run rundevrun guide")
		return
	}
	guide, warning, err := guides.Load(name, opts.noUpdate)
	if err != nil {
		failError(e, err)
		return
	}
	item.Guide = guide
	if warning != "" {
		e.Warnings = append(e.Warnings, warning)
	}
	e.Results = append(e.Results, item)
}

func missingProjectConfig(start string) bool {
	_, file, _, err := discovery.ActiveConfig(start)
	return err == nil && file == ""
}
