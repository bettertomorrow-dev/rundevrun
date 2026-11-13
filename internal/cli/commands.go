package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

type projectInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Root string `json:"root"`
}

func (p projectInfo) String() string { return fmt.Sprintf("%s\t%s\t%s", p.ID, p.Name, p.Root) }

type worktreeInfo struct {
	ID          string `json:"id"`
	ProjectID   string `json:"project_id"`
	Root        string `json:"root"`
	Branch      string `json:"branch,omitempty"`
	Detached    bool   `json:"detached"`
	ConfigValid bool   `json:"config_valid"`
	ConfigError string `json:"config_error,omitempty"`
}

func (w worktreeInfo) String() string {
	line := fmt.Sprintf("%s\t%s\t%s", w.ID, w.Branch, w.Root)
	if w.ConfigError != "" {
		line += "\tconfig error: " + w.ConfigError
	}
	return line
}

type instanceInfo struct {
	ID         string `json:"instance_id"`
	ProjectID  string `json:"project_id"`
	WorktreeID string `json:"worktree_id"`
	Service    string `json:"service"`
	State      string `json:"state"`
	Protocol   string `json:"protocol,omitempty"`
	RunID      string `json:"run_id,omitempty"`
}

func (i instanceInfo) String() string { return fmt.Sprintf("%s\t%s", i.ID, i.State) }

func execute(ctx context.Context, args []string, opts options, e *envelope) {
	if (opts.follow || opts.tailSet) && e.Command != "logs" {
		e.fail("USAGE", "--follow and --tail are only valid with logs")
		return
	}
	if opts.follow && opts.json {
		e.fail("USAGE", "--json --follow is unsupported; omit --follow for one JSON result")
		return
	}
	root, err := runtimeRoot()
	if err != nil {
		failError(e, err)
		return
	}
	store := &state.Store{Root: root}
	switch e.Command {
	case "register":
		if !validArity(args, 1, 2, e) {
			return
		}
		if opts.project != "" || opts.worktree != "" {
			e.fail("USAGE", "register takes a path, not project/worktree selectors")
			return
		}
		path := "."
		if len(args) == 2 {
			path = args[1]
		}
		register(store, path, opts.dryRun, e)
	case "projects", "ls", "status":
		if !validArity(args, 1, 1, e) {
			return
		}
		if opts.dryRun {
			e.fail("USAGE", "--dry-run is only valid for commands that change state")
			return
		}
		if opts.worktree != "" && (opts.project == "" || e.Command == "projects") {
			e.fail("USAGE", "instance listings require --project with --worktree; projects does not accept --worktree")
			return
		}
		listRegistry(store, opts, e)
	case "worktrees", "services", "config check":
		arity := 1
		if e.Command == "config check" {
			arity = 2
		}
		if !validArity(args, arity, arity, e) {
			return
		}
		if opts.dryRun {
			e.fail("USAGE", "discovery already reads without changing state; omit --dry-run")
			return
		}
		listConfiguration(store, opts, e)
	default:
		e.fail("USAGE", "unknown or not yet implemented command: "+e.Command, "see rdr --help")
	}
	if err := ctx.Err(); err != nil && len(e.Errors) == 0 {
		e.fail("CANCELLED", err.Error())
	}
}

func register(store *state.Store, path string, dryRun bool, e *envelope) {
	p, err := discovery.Discover(path)
	if err != nil {
		failError(e, err)
		return
	}
	e.Scope.Project = p.ID
	r, err := store.Read()
	if err != nil {
		failError(e, err)
		return
	}
	if _, err = r.Project(p.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
		failError(e, err)
		return
	}
	e.Results = append(e.Results, projectInfo{p.ID, p.Name, p.Root})
	// A sibling without usable configuration remains discoverable by its ID.
	for _, w := range p.Worktrees {
		info := worktreeInfo{ID: w.ID, ProjectID: p.ID, Root: w.Root, Branch: w.Branch, Detached: w.Detached, ConfigValid: w.Config != nil}
		if w.ConfigError != nil {
			info.ConfigError = w.ConfigError.Error()
			e.fail("CONFIG_INVALID", w.Root+": "+w.ConfigError.Error(), "fix dev.toml in this worktree")
		}
		e.Results = append(e.Results, info)
	}
	if dryRun {
		if len(e.Errors) > 0 {
			e.Status = "partial"
		}
		return
	}
	err = store.Update(func(r *state.Registry) error {
		saved, err := r.Project(p.ID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		saved.ID, saved.Name, saved.Root, saved.GitCommonDir = p.ID, p.Name, p.Root, p.GitCommonDir
		// Historical/live instances still need descriptors of removed worktrees.
		knownWorktrees := make(map[string]state.Worktree)
		for _, w := range saved.Worktrees {
			knownWorktrees[w.ID] = w
		}
		if saved.Instances == nil {
			saved.Instances = make(map[string]state.Instance)
		}
		for _, w := range p.Worktrees {
			knownWorktrees[w.ID] = state.Worktree{ID: w.ID, Root: w.Root, Branch: w.Branch, Detached: w.Detached}
			if w.Config == nil {
				continue
			}
			for name := range w.Config.Services {
				id := p.ID + "/" + w.ID + "/" + name
				if _, exists := saved.Instances[id]; !exists {
					saved.Instances[id] = state.Instance{ID: id, Service: name, WorktreeID: w.ID, State: "stopped"}
				}
			}
		}
		saved.Worktrees = nil
		for _, id := range sortedKeys(knownWorktrees) {
			saved.Worktrees = append(saved.Worktrees, knownWorktrees[id])
		}
		return r.SetProject(p.ID, saved)
	})
	if err != nil {
		e.Results = []any{}
		failError(e, err)
	} else if len(e.Errors) > 0 {
		e.Status = "partial"
	}
}

func selectProject(r state.Registry, selector string, e *envelope) *state.Project {
	if _, ok := r.Projects[selector]; ok {
		p, err := r.Project(selector)
		if err != nil {
			failError(e, err)
			return nil
		}
		return &p
	}
	var matches []state.Project
	for _, id := range sortedKeys(r.Projects) {
		p, err := r.Project(id)
		if err != nil {
			continue
		}
		if p.Name == selector {
			matches = append(matches, p)
		}
	}
	if len(matches) == 0 {
		e.fail("NOT_FOUND", "registered project not found: "+selector, "use rdr projects --json")
		return nil
	}
	if len(matches) > 1 {
		e.fail("AMBIGUOUS", "project name has multiple matches: "+selector, "select --project with an exact ID")
		for _, p := range matches {
			e.Errors[len(e.Errors)-1].Candidates = append(e.Errors[len(e.Errors)-1].Candidates, p.ID)
		}
		return nil
	}
	return &matches[0]
}

func listConfiguration(store *state.Store, opts options, e *envelope) {
	start := "."
	if opts.project != "" {
		r, err := store.Read()
		if err != nil {
			failError(e, err)
			return
		}
		p := selectProject(r, opts.project, e)
		if p == nil {
			return
		}
		start = p.Root
	}
	p, err := discovery.Discover(start)
	if err != nil {
		failError(e, err)
		return
	}
	e.Scope.Project = p.ID
	worktrees := p.Worktrees
	if opts.worktree != "" || e.Command == "config check" {
		selector := opts.worktree
		if selector == "" {
			selector = p.Root
		}
		w, err := discovery.Resolve(p, selector)
		if err != nil {
			failError(e, err)
			return
		}
		worktrees = []discovery.Worktree{*w}
		e.Scope.Worktree = w.ID
	}
	r, err := store.Read()
	if err != nil {
		failError(e, err)
		return
	}
	saved, stateErr := r.Project(p.ID)
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		failError(e, stateErr)
		return
	}
	for _, w := range worktrees {
		if e.Command == "worktrees" {
			info := worktreeInfo{ID: w.ID, ProjectID: p.ID, Root: w.Root, Branch: w.Branch, Detached: w.Detached, ConfigValid: w.Config != nil}
			if w.ConfigError != nil {
				info.ConfigError = w.ConfigError.Error()
			}
			e.Results = append(e.Results, info)
			continue
		}
		if w.ConfigError != nil {
			failError(e, w.ConfigError)
			continue
		}
		if w.Config == nil {
			e.fail("CONFIG_INVALID", "missing dev.toml in "+w.Root)
			continue
		}
		for _, name := range sortedKeys(w.Config.Services) {
			id := p.ID + "/" + w.ID + "/" + name
			info := instanceInfo{ID: id, ProjectID: p.ID, WorktreeID: w.ID, Service: name, State: "stopped", Protocol: w.Config.Services[name].Protocol}
			if instance, ok := saved.Instances[id]; ok {
				info.State, info.RunID = instance.State, instance.RunID
			}
			e.Results = append(e.Results, info)
		}
	}
	if len(e.Errors) > 0 && len(e.Results) > 0 {
		e.Status = "partial"
	}
}

func listRegistry(store *state.Store, opts options, e *envelope) {
	r, err := store.Read()
	if err != nil {
		failError(e, err)
		return
	}
	ids := sortedKeys(r.Projects)
	if opts.project != "" {
		p := selectProject(r, opts.project, e)
		if p == nil {
			return
		}
		ids = []string{p.ID}
		e.Scope.Project = p.ID
	}
	for _, id := range ids {
		p, err := r.Project(id)
		if err != nil {
			failError(e, err)
			continue
		}
		if e.Command == "projects" {
			e.Results = append(e.Results, projectInfo{p.ID, p.Name, p.Root})
			continue
		}
		selectedWorktree := ""
		if opts.worktree != "" {
			d, err := discovery.Discover(p.Root)
			if err != nil {
				failError(e, err)
				continue
			}
			w, err := discovery.Resolve(d, opts.worktree)
			if err != nil {
				failError(e, err)
				continue
			}
			selectedWorktree = w.ID
		}
		for _, key := range sortedKeys(p.Instances) {
			i := p.Instances[key]
			if selectedWorktree != "" && i.WorktreeID != selectedWorktree {
				continue
			}
			e.Results = append(e.Results, instanceInfo{ID: i.ID, ProjectID: p.ID, WorktreeID: i.WorktreeID, Service: i.Service, State: i.State, RunID: i.RunID})
		}
	}
	if len(e.Errors) > 0 && len(e.Results) > 0 {
		e.Status = "partial"
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func failError(e *envelope, err error) {
	code := "IO_ERROR"
	var configErr *config.Error
	switch {
	case errors.Is(err, state.ErrCorrupt):
		code = "STATE_CORRUPT"
	case errors.Is(err, state.ErrBusy):
		code = "BUSY"
	case errors.Is(err, discovery.ErrAmbiguous):
		code = "AMBIGUOUS"
	case errors.Is(err, discovery.ErrNotFound), errors.Is(err, os.ErrNotExist):
		code = "NOT_FOUND"
	case errors.Is(err, config.ErrVersionUnsupported):
		code = "VERSION_UNSUPPORTED"
	case errors.As(err, &configErr):
		code = configErr.Code
	}
	e.fail(code, err.Error())
	if errors.As(err, &configErr) {
		e.Errors[len(e.Errors)-1].Field = configErr.Field
	}
}
