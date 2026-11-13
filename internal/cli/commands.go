package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
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
	ID           string `json:"instance_id"`
	ProjectID    string `json:"project_id"`
	WorktreeID   string `json:"worktree_id"`
	Service      string `json:"service"`
	State        string `json:"state,omitempty"`
	StateUnknown bool   `json:"state_unknown,omitempty"`
	Protocol     string `json:"protocol,omitempty"`
	RunID        string `json:"run_id,omitempty"`
}

func (i instanceInfo) String() string { return fmt.Sprintf("%s\t%s", i.ID, i.State) }

func execute(ctx context.Context, args []string, opts options, e *envelope, out io.Writer) {
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
	case "up", "down", "restart", "logs":
		if !validArity(args, 2, 2, e) {
			return
		}
		if e.Command == "logs" && opts.dryRun {
			e.fail("USAGE", "--dry-run is only valid for lifecycle mutations")
			return
		}
		if e.Command != "logs" && (opts.follow || opts.tailSet) {
			e.fail("USAGE", "--follow and --tail are only valid with logs")
			return
		}
		lifecycle(ctx, store, args[1], opts, e, out)
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

func lifecycle(ctx context.Context, store *state.Store, service string, opts options, e *envelope, stdout io.Writer) {
	r, err := store.Read()
	if err != nil {
		failError(e, err)
		return
	}
	var p state.Project
	if opts.project != "" {
		selected := selectProject(r, opts.project, e)
		if selected == nil {
			return
		}
		p = *selected
	} else {
		cwd, er := os.Getwd()
		if er != nil {
			failError(e, er)
			return
		}
		var matches []state.Project
		cwd, _ = filepath.EvalSymlinks(cwd)
		for _, id := range sortedKeys(r.Projects) {
			saved, er := r.Project(id)
			if er != nil {
				continue
			}
			inside := within(cwd, saved.Root)
			for _, wt := range saved.Worktrees {
				inside = inside || within(cwd, wt.Root)
			}
			if inside {
				matches = append(matches, saved)
			}
		}
		if len(matches) != 1 {
			e.fail("NOT_FOUND", "current directory is not inside one registered project", "run rdr register explicitly or use --project")
			return
		}
		p = matches[0]
	}
	e.Scope.Project = p.ID
	d, err := discovery.Discover(p.Root)
	if err != nil {
		if e.Command != "down" && e.Command != "logs" {
			failError(e, err)
			return
		}
		d = descriptor(p)
	}
	worktreeSelector := opts.worktree
	if worktreeSelector == "" {
		cwd, er := os.Getwd()
		if er != nil {
			failError(e, er)
			return
		}
		worktreeSelector = cwd
	}
	w, err := discovery.ResolveDescriptor(d, worktreeSelector)
	if err != nil {
		failError(e, err)
		return
	}
	e.Scope.Worktree = w.ID
	if (e.Command == "up" || e.Command == "restart") && w.Config == nil {
		if w.ConfigError != nil {
			failError(e, w.ConfigError)
		} else {
			e.fail("CONFIG_INVALID", "missing dev.toml in "+w.Root)
		}
		return
	}
	runnerInstance := runner.New(store, executablePath())
	if e.Command == "logs" {
		var logOut io.Writer = stdout
		captured := &cappedWriter{limit: 1 << 20}
		if opts.json {
			logOut = captured
		}
		err = runnerInstance.Logs(ctx, d, w, service, opts.tail, opts.follow, logOut)
		if err != nil {
			failError(e, err)
			return
		}
		if opts.json {
			e.Results = append(e.Results, map[string]any{"service": service, "logs": captured.String(), "logs_truncated": captured.truncated})
		}
		return
	}
	action := e.Command
	if opts.dryRun {
		plan, er := runnerInstance.Plan(ctx, action, d, w, service)
		if er != nil {
			failError(e, er)
			return
		}
		e.Plan = plan
		return
	}
	var result runner.Result
	switch action {
	case "up":
		result, err = runnerInstance.Up(ctx, d, w, service)
	case "down":
		result, err = runnerInstance.Stop(ctx, d, w, service)
	case "restart":
		result, err = runnerInstance.Restart(ctx, d, w, service)
	}
	if err != nil {
		var runErr *runner.Error
		if errors.Is(err, context.Canceled) {
			e.fail("CANCELLED", err.Error())
		} else if errors.As(err, &runErr) {
			e.fail(runErr.Code, err.Error())
		} else {
			failError(e, err)
		}
		id := p.ID + "/" + w.ID + "/" + service
		stateName := ""
		if reg, readErr := store.Read(); readErr == nil {
			if saved, projectErr := reg.Project(p.ID); projectErr == nil {
				if instance, ok := saved.Instances[id]; ok {
					if observed, inspectErr := store.Inspect(instance); inspectErr == nil {
						stateName = observed
						if stateName == "stale" {
							stateName = "failed"
						}
					}
				}
			}
		}
		if stateName == "" {
			e.Results = append(e.Results, map[string]any{"instance_id": id, "status": "failed", "state_unknown": true})
		} else {
			e.Results = append(e.Results, runner.Result{InstanceID: id, State: stateName, Status: "failed"})
		}
		return
	}
	e.Results = append(e.Results, result)
}

type cappedWriter struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	n := len(p)
	remaining := w.limit - w.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
			w.truncated = true
		}
		_, _ = w.Buffer.Write(p)
	} else if n > 0 {
		w.truncated = true
	}
	return n, nil
}

func executablePath() string { path, _ := os.Executable(); return path }

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
				info.State, info.StateUnknown = inspectState(store, instance, e)
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
			observed, unknown := inspectState(store, i, e)
			e.Results = append(e.Results, instanceInfo{ID: i.ID, ProjectID: p.ID, WorktreeID: i.WorktreeID, Service: i.Service, State: observed, StateUnknown: unknown, RunID: i.RunID})
		}
	}
	if len(e.Errors) > 0 && len(e.Results) > 0 {
		e.Status = "partial"
	}
}

func inspectState(store *state.Store, instance state.Instance, e *envelope) (string, bool) {
	observed, err := store.Inspect(instance)
	if err != nil {
		e.fail("OWNERSHIP_UNKNOWN", err.Error(), "inspect the run state before retrying a mutation")
		e.Errors[len(e.Errors)-1].InstanceID = instance.ID
		return "", true
	}
	if observed == "stale" {
		observed = "failed"
	}
	return observed, false
}

func descriptor(p state.Project) *discovery.Project {
	d := &discovery.Project{ID: p.ID, Name: p.Name, Root: p.Root, GitCommonDir: p.GitCommonDir}
	for _, w := range p.Worktrees {
		d.Worktrees = append(d.Worktrees, discovery.Worktree{ID: w.ID, Root: w.Root, Branch: w.Branch, Detached: w.Detached})
	}
	return d
}

func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
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
	var runErr *runner.Error
	switch {
	case errors.Is(err, context.Canceled):
		code = "CANCELLED"
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
	case errors.As(err, &runErr):
		code = runErr.Code
	}
	e.fail(code, err.Error())
	if errors.As(err, &configErr) {
		e.Errors[len(e.Errors)-1].Field = configErr.Field
	}
}
