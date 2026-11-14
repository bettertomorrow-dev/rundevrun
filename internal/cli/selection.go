package cli

import (
	"os"
	"path/filepath"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

// resolveCommandSelection uses registered metadata first. Reading an unrelated
// worktree's broken configuration must not prevent stopping a saved run.
func resolveCommandSelection(store *state.Store, opts options, requireConfig bool, e *envelope) (*discovery.Project, *discovery.Worktree) {
	r, err := store.Read()
	if err != nil {
		failError(e, err)
		return nil, nil
	}
	cwd, err := os.Getwd()
	if err == nil {
		cwd, err = filepath.EvalSymlinks(cwd)
	}
	if err != nil {
		failError(e, err)
		return nil, nil
	}
	var p *state.Project
	if opts.project != "" {
		p = selectProject(r, opts.project, e)
	} else {
		depth := -1
		ambiguous := false
		for _, id := range sortedKeys(r.Projects) {
			saved, err := r.Project(id)
			if err != nil {
				continue
			}
			for _, w := range saved.Worktrees {
				if !within(cwd, w.Root) || len(w.Root) < depth {
					continue
				}
				if len(w.Root) == depth && p != nil && p.ID != saved.ID {
					ambiguous = true
					continue
				}
				p, depth, ambiguous = &saved, len(w.Root), false
			}
		}
		if ambiguous {
			e.fail("AMBIGUOUS", "multiple registered projects contain the current directory", "select --project with an exact ID")
			return nil, nil
		}
		if p == nil {
			e.fail("USAGE", "outside a registered project, specify --project and --worktree", "register the project explicitly if needed")
		}
	}
	if p == nil {
		return nil, nil
	}
	d := descriptor(*p)
	selector := opts.worktree
	if selector == "" {
		// Commands invoked below the root still select the containing worktree.
		for _, w := range p.Worktrees {
			if within(cwd, w.Root) && (selector == "" || len(w.Root) > len(selector)) {
				selector = w.Root
			}
		}
		if selector == "" {
			e.fail("USAGE", "outside the selected project, specify --worktree")
			return nil, nil
		}
	}
	if requireConfig {
		// Refresh branch labels and find a usable configuration without requiring
		// an unrelated registered worktree's current dev.toml to be valid.
		roots := []string{p.Root}
		for _, w := range p.Worktrees {
			roots = append(roots, w.Root)
		}
		var firstErr error
		for _, root := range roots {
			fresh, err := discovery.Discover(root)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if fresh.ID != p.ID {
				e.fail("STATE_CONFLICT", "registered project identity changed")
				return nil, nil
			}
			w, err := discovery.Resolve(fresh, selector)
			if err != nil {
				failError(e, err)
				return nil, nil
			}
			e.Scope = scope{Project: fresh.ID, Worktree: w.ID}
			return fresh, w
		}
		failError(e, firstErr)
		return nil, nil
	}
	w, err := discovery.ResolveDescriptor(d, selector)
	if err != nil {
		failError(e, err)
		return nil, nil
	}
	e.Scope = scope{Project: d.ID, Worktree: w.ID}
	return d, w
}
