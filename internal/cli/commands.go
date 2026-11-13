package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
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
	ID                string            `json:"instance_id"`
	ProjectID         string            `json:"project_id"`
	WorktreeID        string            `json:"worktree_id"`
	Service           string            `json:"service"`
	State             string            `json:"state,omitempty"`
	StateUnknown      bool              `json:"state_unknown,omitempty"`
	Protocol          string            `json:"protocol,omitempty"`
	RunID             string            `json:"run_id,omitempty"`
	ConfigChanged     bool              `json:"config_changed"`
	RestartRequired   bool              `json:"restart_required"`
	DesiredBindings   map[string]string `json:"desired_bindings,omitempty"`
	EffectiveBindings map[string]string `json:"effective_bindings,omitempty"`
	DependencyError   string            `json:"dependency_error,omitempty"`
	ConfigError       string            `json:"config_error,omitempty"`
	Preparation       *preparationInfo  `json:"preparation,omitempty"`
	Hook              *hookInfo         `json:"hook,omitempty"`
	PID               int               `json:"pid,omitempty"`
	DirectURL         string            `json:"direct_url,omitempty"`
	URL               string            `json:"url,omitempty"`
	LogPaths          map[string]string `json:"log_paths,omitempty"`
}

type preparationInfo struct {
	RunID        string `json:"run_id"`
	Phase        string `json:"phase"`
	BuildLogPath string `json:"build_log_path"`
}

type hookInfo struct {
	RunID       string `json:"run_id"`
	OperationID string `json:"operation_id"`
	Name        string `json:"name"`
	Phase       string `json:"phase"`
	LogPath     string `json:"log_path"`
}

func (i instanceInfo) String() string { return fmt.Sprintf("%s\t%s", i.ID, i.State) }

func execute(ctx context.Context, args []string, opts options, e *envelope, out io.Writer) {
	if opts.all && e.Command != "down" {
		e.fail("USAGE", "--all is only valid with down")
		return
	}
	if opts.cascade && e.Command != "down" {
		e.fail("USAGE", "--cascade is only valid with down")
		return
	}
	if opts.retryHooks && e.Command != "up" && e.Command != "down" && e.Command != "restart" && e.Command != "switch" {
		e.fail("USAGE", "--retry-hooks is only valid with up, down, restart, or switch")
		return
	}
	if opts.toWorktree != "" && e.Command != "bind" && e.Command != "switch" {
		e.fail("USAGE", "--to-worktree is only supported by bind or switch")
		return
	}
	if (opts.file != "" || opts.allServices) && e.Command != "switch" {
		e.fail("USAGE", "--file and --all-services are only valid with switch")
		return
	}
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
	case "switch":
		switchCommand(ctx, store, args[1:], opts, e)
	case "bind":
		if !validArity(args, 3, 3, e) {
			return
		}
		if opts.toWorktree == "" {
			e.fail("USAGE", "bind requires --to-worktree", "see rdr --help")
			return
		}
		d, w := resolveCommandSelection(store, opts, true, e)
		if d == nil {
			return
		}
		target, err := discovery.Resolve(d, opts.toWorktree)
		if err != nil {
			failError(e, err)
			return
		}
		r := runner.New(store, executablePath())
		consumer := runner.Selection{Project: d, Worktree: w, Service: args[1]}
		dependency := runner.Selection{Project: d, Worktree: target, Service: args[2]}
		var bound runner.BindResult
		if opts.dryRun {
			bound, err = r.PlanBind(ctx, consumer, args[2], dependency)
		} else {
			bound, err = r.Bind(ctx, consumer, args[2], dependency)
		}
		if err != nil {
			failError(e, err)
			return
		}
		if opts.dryRun {
			e.Plan = bound
		} else {
			e.Results = append(e.Results, bound)
		}
	case "up", "down", "restart", "logs":
		if e.Command == "logs" {
			if !validArity(args, 2, 2, e) {
				return
			}
		} else if !validLifecycleArity(args, e.Command, e) {
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
		if e.Command == "logs" {
			logsCommand(ctx, store, args[1], opts, e, out)
		} else {
			lifecycleCommand(ctx, store, args[1:], opts, e)
		}
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

type switchFile struct {
	Version  int                 `json:"version"`
	Mappings []switchFileMapping `json:"mappings"`
}
type switchFileMapping struct {
	From switchFileSelection `json:"from"`
	To   switchFileSelection `json:"to"`
}
type switchFileSelection struct {
	Project  string `json:"project"`
	Worktree string `json:"worktree"`
	Service  string `json:"service"`
}

func switchCommand(ctx context.Context, store *state.Store, services []string, opts options, e *envelope) {
	if (opts.file != "") == (opts.toWorktree != "") || (opts.file != "" && (opts.project != "" || opts.worktree != "" || opts.allServices || len(services) > 0)) {
		e.fail("USAGE", "switch requires either selectors with --to-worktree or --file; file mode cannot mix selectors")
		return
	}
	if opts.file == "" && len(services) == 0 && !opts.allServices {
		e.fail("USAGE", "switch requires service selectors or --all-services")
		return
	}
	if opts.allServices && len(services) != 0 {
		e.fail("USAGE", "--all-services cannot be combined with service names")
		return
	}
	var mappings []runner.SwitchMapping
	var f switchFile
	if opts.file != "" {
		var err error
		f, err = readSwitchFile(opts.file)
		if err != nil {
			failError(e, err)
			return
		}
	}
	reg, err := store.Read()
	if err != nil {
		failError(e, err)
		return
	}
	resolve := func(s switchFileSelection) (runner.Selection, error) {
		p := selectProject(reg, s.Project, e)
		if p == nil {
			return runner.Selection{}, errors.New("invalid project")
		}
		d, wt := resolveCommandSelection(store, options{project: p.ID, worktree: s.Worktree}, true, e)
		if d == nil {
			return runner.Selection{}, errors.New("invalid worktree selection")
		}
		if _, ok := wt.Config.Services[s.Service]; !ok {
			return runner.Selection{}, &runner.Error{Code: "NOT_FOUND", Err: fmt.Errorf("service not found: %s", s.Service)}
		}
		return runner.Selection{Project: d, Worktree: wt, Service: s.Service}, nil
	}
	if opts.file != "" {
		for _, m := range f.Mappings {
			from, er := resolve(m.From)
			if er != nil {
				if len(e.Errors) == 0 {
					failError(e, er)
				}
				return
			}
			to, er := resolve(m.To)
			if er != nil {
				if len(e.Errors) == 0 {
					failError(e, er)
				}
				return
			}
			mappings = append(mappings, runner.SwitchMapping{From: from, To: to})
		}
		e.Scope = scope{}
	} else {
		d, w := resolveCommandSelection(store, opts, true, e)
		if d == nil {
			return
		}
		target, err := discovery.Resolve(d, opts.toWorktree)
		if err != nil {
			failError(e, err)
			return
		}
		if opts.allServices {
			instances := mustProjectInstances(store, d.ID, e)
			for _, id := range sortedKeys(instances) {
				inst := instances[id]
				if inst.WorktreeID == w.ID {
					obs, unknown := inspectState(store, inst, e)
					if !unknown && liveState(obs) {
						mappings = append(mappings, runner.SwitchMapping{From: runner.Selection{Project: d, Worktree: w, Service: inst.Service}, To: runner.Selection{Project: d, Worktree: target, Service: inst.Service}})
					}
				}
			}
		} else {
			for _, service := range services {
				if _, ok := w.Config.Services[service]; !ok {
					e.fail("NOT_FOUND", "service not found: "+service)
					return
				}
				mappings = append(mappings, runner.SwitchMapping{From: runner.Selection{Project: d, Worktree: w, Service: service}, To: runner.Selection{Project: d, Worktree: target, Service: service}})
			}
		}
	}
	if len(e.Errors) > 0 {
		return
	}
	r := runner.New(store, executablePath())
	r.RetryHooks = opts.retryHooks
	if opts.dryRun {
		plan, err := r.PlanSwitch(ctx, mappings)
		if err != nil {
			failError(e, err)
			return
		}
		e.Plan = plan
		return
	}
	out, err := r.SwitchGraph(ctx, mappings)
	if err != nil && len(out.Errors) == 0 {
		failError(e, err)
	}
	if out.Status != "" {
		e.Status = out.Status
	}
	for _, item := range out.Results {
		e.Results = append(e.Results, item)
	}
	e.OperationID = out.OperationID
	e.JournalPath = out.JournalPath
	for _, step := range out.Steps {
		e.Steps = append(e.Steps, operationStepView{ID: step.ID, Phase: step.Status, Status: terminalStepStatus(step.Status), Code: step.Code, Message: step.Message})
	}
	for _, ge := range out.Errors {
		e.Errors = append(e.Errors, commandError{Code: ge.Code, Message: ge.Message, InstanceID: ge.InstanceID, Actions: []string{}})
	}
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		for _, failure := range e.Errors {
			if failure.Code == "CANCELLED" {
				return
			}
		}
		e.Errors = append(e.Errors, commandError{Code: "CANCELLED", Message: "switch cancelled", Actions: []string{}})
	}
}

func readSwitchFile(path string) (switchFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return switchFile{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return switchFile{}, err
	}
	if len(data) > 1<<20 {
		return switchFile{}, &runner.Error{Code: "USAGE", Err: errors.New("switch file exceeds 1 MiB")}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f switchFile
	if err := dec.Decode(&f); err != nil {
		return f, &runner.Error{Code: "USAGE", Err: fmt.Errorf("invalid switch file: %w", err)}
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return f, &runner.Error{Code: "USAGE", Err: errors.New("switch file must contain one JSON value")}
	}
	if f.Version != 1 {
		return f, &runner.Error{Code: "VERSION_UNSUPPORTED", Err: errors.New("switch file requires version 1")}
	}
	if len(f.Mappings) == 0 {
		return f, &runner.Error{Code: "USAGE", Err: errors.New("switch file requires nonempty mappings")}
	}
	for _, m := range f.Mappings {
		if m.From.Project == "" || m.From.Worktree == "" || m.From.Service == "" || m.To.Project == "" || m.To.Worktree == "" || m.To.Service == "" {
			return f, &runner.Error{Code: "USAGE", Err: errors.New("each mapping requires complete from and to selections")}
		}
	}
	return f, nil
}

func liveState(s string) bool {
	return s == "starting" || s == "running" || s == "ready" || s == "unhealthy"
}
func mustProjectInstances(store *state.Store, id string, e *envelope) map[string]state.Instance {
	r, err := store.Read()
	if err != nil {
		failError(e, err)
		return nil
	}
	p, err := r.Project(id)
	if err != nil {
		failError(e, err)
		return nil
	}
	return p.Instances
}

func validLifecycleArity(args []string, command string, e *envelope) bool {
	min := 2
	if command == "down" {
		min = 1
	}
	if len(args) < min {
		e.fail("USAGE", "missing service selector", "see rdr --help")
		return false
	}
	return true
}

func lifecycleCommand(ctx context.Context, store *state.Store, services []string, opts options, e *envelope) {
	if opts.all && (opts.project != "" || opts.worktree != "" || len(services) != 0) {
		e.fail("USAGE", "down --all cannot be combined with --project, --worktree, or service selectors")
		return
	}
	if opts.all {
		selections, revision, err := allDownSelections(store)
		if err != nil {
			failError(e, err)
			return
		}
		if len(selections) == 0 {
			if opts.dryRun {
				e.Plan = runner.GraphPlan{Action: "down", Revision: revision, Instances: []runner.PlannedInstance{}}
			} else {
				e.Results = append(e.Results, map[string]any{"status": "unchanged"})
			}
			return
		}
		if opts.dryRun {
			setGraphPlan(ctx, store, "down", selections, opts.cascade, revision, e)
			return
		}
		runGraph(ctx, store, "down", selections, opts.cascade, opts.retryHooks, e)
		return
	}
	if e.Command == "down" && len(services) == 0 {
		selections, revision, err := currentDownSelections(store, opts, e)
		if err != nil || len(e.Errors) != 0 {
			if err != nil {
				failError(e, err)
			}
			return
		}
		if len(selections) == 0 {
			if opts.dryRun {
				e.Plan = runner.GraphPlan{Action: "down", Revision: revision, Instances: []runner.PlannedInstance{}}
			} else {
				e.Results = append(e.Results, map[string]any{"status": "unchanged"})
			}
			return
		}
		if opts.dryRun {
			setGraphPlan(ctx, store, "down", selections, opts.cascade, revision, e)
			return
		}
		runGraph(ctx, store, "down", selections, opts.cascade, opts.retryHooks, e)
		return
	}

	if len(services) == 0 {
		e.fail("USAGE", "at least one service is required")
		return
	}
	seen := make(map[string]bool, len(services))
	for _, service := range services {
		if seen[service] {
			e.fail("USAGE", "duplicate service selector: "+service)
			return
		}
		seen[service] = true
	}
	requireConfig := e.Command != "down"
	d, w := resolveCommandSelection(store, opts, requireConfig, e)
	if d == nil {
		return
	}
	reg, err := store.Read()
	if err != nil {
		failError(e, err)
		return
	}
	p, err := reg.Project(d.ID)
	if err != nil {
		failError(e, err)
		return
	}
	selections := make([]runner.Selection, 0, len(services))
	for _, service := range services {
		if requireConfig {
			if _, ok := w.Config.Services[service]; !ok {
				e.fail("NOT_FOUND", "service not found: "+service)
				return
			}
		} else if _, ok := p.Instances[d.ID+"/"+w.ID+"/"+service]; !ok {
			e.fail("NOT_FOUND", "registered service instance not found: "+service)
			return
		}
		selections = append(selections, runner.Selection{Project: d, Worktree: w, Service: service})
	}
	if opts.dryRun {
		setGraphPlan(ctx, store, e.Command, selections, opts.cascade, reg.Revision, e)
		return
	}
	runGraph(ctx, store, e.Command, selections, opts.cascade, opts.retryHooks, e)
}

func setGraphPlan(ctx context.Context, store *state.Store, action string, selections []runner.Selection, cascade bool, revision uint64, e *envelope) {
	if len(selections) == 0 {
		e.Plan = runner.GraphPlan{Action: action, Revision: revision, Instances: []runner.PlannedInstance{}}
		return
	}
	plan, err := runner.New(store, executablePath()).PlanGraph(ctx, action, selections, cascade)
	if err != nil {
		failError(e, err)
		return
	}
	e.Plan = plan
}

func runGraph(ctx context.Context, store *state.Store, action string, selections []runner.Selection, cascade, retryHooks bool, e *envelope) {
	r := runner.New(store, executablePath())
	if retryHooks {
		r.RetryHooks = true
	}
	var result runner.GraphResult
	var err error
	switch action {
	case "up":
		result, err = r.UpGraph(ctx, selections)
	case "down":
		result, err = r.DownGraph(ctx, selections, cascade)
	case "restart":
		result, err = r.RestartGraph(ctx, selections)
	default:
		e.fail("USAGE", "unsupported graph action: "+action)
		return
	}
	for _, item := range result.Results {
		if action == "up" {
			e.Results = append(e.Results, graphInstanceInfo(store, item, e))
		} else {
			e.Results = append(e.Results, item)
		}
	}
	for _, failure := range result.Errors {
		e.Errors = append(e.Errors, commandError{Code: failure.Code, Message: failure.Message, InstanceID: failure.InstanceID, Actions: []string{}})
	}
	if err != nil {
		code := graphFailureCode(ctx, err)
		appendError := len(result.Errors) == 0
		if code == "CANCELLED" {
			appendError = true
			for _, existing := range e.Errors {
				if existing.Code == "CANCELLED" {
					appendError = false
					break
				}
			}
		}
		if appendError {
			e.Errors = append(e.Errors, commandError{Code: code, Message: err.Error(), Actions: []string{}})
		}
	}
	switch result.Status {
	case "partial":
		e.Status = "partial"
	case "error":
		e.Status = "error"
	default:
		e.Status = "ok"
		if len(e.Errors) > 0 {
			succeeded := false
			for _, raw := range e.Results {
				if item, ok := raw.(map[string]any); ok && (item["status"] == "completed" || item["status"] == "unchanged") {
					succeeded = true
					break
				}
				if item, ok := raw.(runner.Result); ok && (item.Status == "completed" || item.Status == "unchanged") {
					succeeded = true
					break
				}
			}
			if succeeded {
				e.Status = "partial"
			} else {
				e.Status = "error"
			}
		}
	}
}

func graphFailureCode(ctx context.Context, err error) string {
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return "CANCELLED"
	}
	var runErr *runner.Error
	if errors.As(err, &runErr) {
		return runErr.Code
	}
	return "PROCESS_FAILED"
}

func graphInstanceInfo(store *state.Store, result runner.Result, e *envelope) map[string]any {
	fields := map[string]any{
		"instance_id": result.InstanceID,
		"status":      result.Status,
		"run_id":      result.RunID,
		"urls":        result.URLs,
		"log_paths":   result.LogPaths,
	}
	if result.StateUnknown || result.State == "" {
		fields["state_unknown"] = true
	} else {
		fields["state"] = result.State
	}
	if result.InstanceID == "" {
		return fields
	}
	parts := strings.Split(result.InstanceID, "/")
	if len(parts) != 3 {
		addGraphInstanceError(e, result.InstanceID, "STATE_CORRUPT", "runner returned an invalid instance ID")
		return fields
	}
	reg, err := store.Read()
	if err != nil {
		addGraphInstanceError(e, result.InstanceID, "IO_ERROR", "cannot read status after up: "+err.Error())
		return fields
	}
	project, err := reg.Project(parts[0])
	if err != nil {
		addGraphInstanceError(e, result.InstanceID, "NOT_FOUND", "project disappeared after up: "+parts[0])
		return fields
	}
	instance, ok := project.Instances[result.InstanceID]
	if !ok {
		addGraphInstanceError(e, result.InstanceID, "NOT_FOUND", "instance metadata disappeared after up")
		return fields
	}
	if hook := hookView(store, instance.Hook); hook != nil {
		fields["hook"] = hook
	}
	if result.StateUnknown {
		return fields
	}
	instance.State = result.State
	if result.RunID != "" {
		instance.RunID = result.RunID
	}
	worktree, ok := findWorktree(project, instance.WorktreeID)
	if !ok {
		addGraphInstanceError(e, result.InstanceID, "NOT_FOUND", "worktree metadata disappeared after up")
		return fields
	}
	cfg, err := config.Load(filepath.Join(worktree.Root, "dev.toml"))
	if err != nil {
		fields["config_error"] = err.Error()
		return fields
	}
	service, ok := cfg.Services[instance.Service]
	if !ok {
		fields["config_error"] = "service is absent from current dev.toml"
		return fields
	}
	changed, restartRequired := configDrift(store, instance, cfg.Digest, e)
	info := instanceInfo{ID: instance.ID, ProjectID: project.ID, WorktreeID: worktree.ID, Service: instance.Service, State: result.State, StateUnknown: result.StateUnknown, RunID: instance.RunID, ConfigChanged: changed, RestartRequired: restartRequired}
	addDependencyDetails(store, project, worktree, &service, instance, true, &info, e)
	fields["config_changed"] = info.ConfigChanged
	fields["restart_required"] = info.RestartRequired
	if len(info.DesiredBindings) > 0 {
		fields["desired_bindings"] = info.DesiredBindings
	}
	if len(info.EffectiveBindings) > 0 {
		fields["effective_bindings"] = info.EffectiveBindings
	}
	if info.DependencyError != "" {
		fields["dependency_error"] = info.DependencyError
	}
	return fields
}

func addGraphInstanceError(e *envelope, instanceID, code, message string) {
	e.fail(code, message)
	e.Errors[len(e.Errors)-1].InstanceID = instanceID
}

func currentDownSelections(store *state.Store, opts options, e *envelope) ([]runner.Selection, uint64, error) {
	d, w := resolveCommandSelection(store, opts, false, e)
	if d == nil {
		return nil, 0, nil
	}
	reg, err := store.Read()
	if err != nil {
		return nil, 0, err
	}
	p, err := reg.Project(d.ID)
	if err != nil {
		return nil, 0, err
	}
	selections := make([]runner.Selection, 0)
	for _, id := range sortedKeys(p.Instances) {
		i := p.Instances[id]
		if i.WorktreeID == w.ID {
			if id != d.ID+"/"+i.WorktreeID+"/"+i.Service || i.Service == "" {
				return nil, 0, fmt.Errorf("registered instance metadata mismatch: %s", id)
			}
			selections = append(selections, runner.Selection{Project: d, Worktree: w, Service: i.Service})
		}
	}
	return selections, reg.Revision, nil
}

func allDownSelections(store *state.Store) ([]runner.Selection, uint64, error) {
	reg, err := store.Read()
	if err != nil {
		return nil, 0, err
	}
	selections := make([]runner.Selection, 0)
	for _, projectID := range sortedKeys(reg.Projects) {
		p, err := reg.Project(projectID)
		if err != nil {
			return nil, 0, err
		}
		d := descriptor(p)
		for _, id := range sortedKeys(p.Instances) {
			i := p.Instances[id]
			if id != p.ID+"/"+i.WorktreeID+"/"+i.Service || i.Service == "" {
				return nil, 0, fmt.Errorf("registered instance metadata mismatch: %s", id)
			}
			w, ok := findWorktree(p, i.WorktreeID)
			if !ok || i.Service == "" {
				return nil, 0, fmt.Errorf("registered instance %s has incomplete worktree metadata", id)
			}
			selections = append(selections, runner.Selection{Project: d, Worktree: &discovery.Worktree{ID: w.ID, Root: w.Root, Branch: w.Branch, Detached: w.Detached}, Service: i.Service})
		}
	}
	return selections, reg.Revision, nil
}

func logsCommand(ctx context.Context, store *state.Store, service string, opts options, e *envelope, stdout io.Writer) {
	d, w := resolveCommandSelection(store, opts, false, e)
	if d == nil {
		return
	}
	var logOut io.Writer = stdout
	captured := &cappedWriter{limit: 1 << 20}
	if opts.json {
		logOut = captured
	}
	err := runner.New(store, executablePath()).Logs(ctx, d, w, service, opts.tail, opts.follow, logOut)
	if err != nil {
		failError(e, err)
		return
	}
	if opts.json {
		e.Results = append(e.Results, map[string]any{"service": service, "logs": captured.String(), "logs_truncated": captured.truncated})
	}
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
	viewProject := state.Project{ID: p.ID, Name: p.Name, Root: p.Root, GitCommonDir: p.GitCommonDir}
	for _, wt := range p.Worktrees {
		viewProject.Worktrees = append(viewProject.Worktrees, state.Worktree{ID: wt.ID, Root: wt.Root, Branch: wt.Branch, Detached: wt.Detached})
	}
	registered := stateErr == nil
	if registered {
		viewProject = saved
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
			service := w.Config.Services[name]
			info := instanceInfo{ID: id, ProjectID: p.ID, WorktreeID: w.ID, Service: name, State: "stopped", Protocol: service.Protocol}
			instance := state.Instance{ID: id, Service: name, WorktreeID: w.ID}
			if savedInstance, ok := saved.Instances[id]; ok {
				instance = savedInstance
				info.State, info.RunID = instance.State, instance.RunID
				info.State, info.StateUnknown = inspectState(store, instance, e)
				instance.State = info.State
				info.ConfigChanged, info.RestartRequired = configDrift(store, instance, w.Config.Digest, e)
				info.Preparation = preparationView(store, instance)
				info.Hook = hookView(store, instance.Hook)
				addRunDetails(store, instance, &info)
			}
			addDependencyDetails(store, viewProject, state.Worktree{ID: w.ID, Root: w.Root}, &service, instance, registered, &info, e)
			e.Results = append(e.Results, info)
		}
	}
	if len(e.Errors) > 0 && len(e.Results) > 0 {
		e.Status = "partial"
	}
}

type operationView struct {
	ID           string              `json:"id"`
	Kind         string              `json:"kind"`
	Status       string              `json:"status"`
	OwnerLive    *bool               `json:"owner_live,omitempty"`
	StateUnknown bool                `json:"operation_state_unknown,omitempty"`
	Interrupted  *bool               `json:"operation_interrupted,omitempty"`
	Steps        []operationStepView `json:"steps"`
}

type operationStepView struct {
	ID      string `json:"id"`
	Phase   string `json:"phase"`
	Status  string `json:"status,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func terminalStepStatus(s string) string {
	if s == "completed" || s == "failed" || s == "not_run" || s == "interrupted" {
		return s
	}
	return ""
}

func addOperationViews(store *state.Store, projects []string, e *envelope) {
	ops, err := store.ListOperations()
	if err != nil {
		failError(e, err)
		return
	}
	wanted := map[string]bool{}
	for _, id := range projects {
		wanted[id] = true
	}
	for _, op := range ops {
		var p struct {
			Mappings []struct {
				From string `json:"from"`
				To   string `json:"to"`
			} `json:"mappings"`
		}
		if err := json.Unmarshal(op.Plan, &p); err != nil {
			e.fail("STATE_CORRUPT", "invalid switch operation plan: "+op.ID)
			continue
		}
		relevant := len(projects) == 0
		for _, m := range p.Mappings {
			if wanted[strings.SplitN(m.From, "/", 2)[0]] || wanted[strings.SplitN(m.To, "/", 2)[0]] {
				relevant = true
			}
		}
		if !relevant {
			continue
		}
		view := operationView{ID: op.ID, Kind: op.Kind, Status: op.Status}
		for _, step := range op.Steps {
			view.Steps = append(view.Steps, operationStepView{ID: step.ID, Phase: step.Status, Status: terminalStepStatus(step.Status), Code: step.Code, Message: step.Message})
		}
		live, checkErr := procid.Check(op.Owner)
		if checkErr == nil {
			view.OwnerLive = &live
			if !live {
				interrupted := op.Status != "completed" && op.Status != "failed"
				view.Interrupted = &interrupted
			}
		} else {
			view.StateUnknown = true
			e.Errors = append(e.Errors, commandError{Code: "OWNERSHIP_UNKNOWN", Message: "cannot verify switch operation owner: " + op.ID, Actions: []string{}})
		}
		e.Operations = append(e.Operations, view)
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
			w, err := discovery.ResolveDescriptor(descriptor(p), opts.worktree)
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
			info := instanceInfo{ID: i.ID, ProjectID: p.ID, WorktreeID: i.WorktreeID, Service: i.Service, State: observed, StateUnknown: unknown, RunID: i.RunID}
			info.Preparation = preparationView(store, i)
			info.Hook = hookView(store, i.Hook)
			addRunDetails(store, i, &info)
			i.State = observed
			if worktree, ok := findWorktree(p, i.WorktreeID); ok {
				cfg, configErr := config.Load(filepath.Join(worktree.Root, "dev.toml"))
				if configErr != nil {
					info.ConfigError = configErr.Error()
					addDependencyDetails(store, p, worktree, nil, i, true, &info, e)
				} else {
					service, exists := cfg.Services[i.Service]
					if exists {
						info.Protocol = service.Protocol
						info.ConfigChanged, info.RestartRequired = configDrift(store, i, cfg.Digest, e)
						addDependencyDetails(store, p, worktree, &service, i, true, &info, e)
					} else {
						info.ConfigError = "service is absent from current dev.toml"
						info.ConfigChanged, info.RestartRequired = configDrift(store, i, cfg.Digest, e)
						addDependencyDetails(store, p, worktree, nil, i, true, &info, e)
					}
				}
			} else {
				info.ConfigError = "registered worktree metadata is missing"
				addDependencyDetails(store, p, state.Worktree{ID: i.WorktreeID}, nil, i, true, &info, e)
			}
			e.Results = append(e.Results, info)
		}
	}
	if len(e.Errors) > 0 && len(e.Results) > 0 {
		e.Status = "partial"
	}
	if e.Command == "status" || e.Command == "ls" {
		addOperationViews(store, ids, e)
	}
}

func preparationView(store *state.Store, i state.Instance) *preparationInfo {
	if i.Preparation == nil {
		return nil
	}
	return &preparationInfo{RunID: i.Preparation.RunID, Phase: i.Preparation.Phase, BuildLogPath: filepath.Join(store.Root, "runs", i.Preparation.RunID, "build.log")}
}

func hookView(store *state.Store, hook *state.HookIntent) *hookInfo {
	if hook == nil {
		return nil
	}
	return &hookInfo{
		RunID:       hook.RunID,
		OperationID: hook.OperationID,
		Name:        hook.Name,
		Phase:       hook.Phase,
		LogPath:     filepath.Join(store.Root, "runs", hook.RunID, "hook-"+hook.Name+".log"),
	}
}

func addRunDetails(store *state.Store, instance state.Instance, info *instanceInfo) {
	if instance.RunID == "" || info.StateUnknown {
		return
	}
	manifest, err := store.ReadManifest(instance.RunID)
	if err != nil {
		return
	} // inspectState already reports invalid run records.
	info.DirectURL, info.URL = manifest.Addresses["direct_url"], manifest.Addresses["url"]
	base := filepath.Join(store.Root, "runs", instance.RunID)
	info.LogPaths = map[string]string{"build": filepath.Join(base, "build.log"), "application": filepath.Join(base, "application.log"), "supervisor": filepath.Join(base, "supervisor.log")}
	if info.State == "starting" || info.State == "running" || info.State == "ready" || info.State == "unhealthy" {
		if record, err := store.ReadRun(instance.RunID); err == nil {
			info.PID = record.Child.PID
		}
	}
}

func instanceByID(store *state.Store, projectID, instanceID string) state.Instance {
	r, err := store.Read()
	if err != nil {
		return state.Instance{}
	}
	p, err := r.Project(projectID)
	if err != nil {
		return state.Instance{}
	}
	return p.Instances[instanceID]
}

func findWorktree(p state.Project, id string) (state.Worktree, bool) {
	for _, w := range p.Worktrees {
		if w.ID == id {
			return w, true
		}
	}
	return state.Worktree{}, false
}

func configDrift(store *state.Store, instance state.Instance, currentDigest string, e *envelope) (bool, bool) {
	if instance.RunID == "" || (instance.State != "running" && instance.State != "ready" && instance.State != "unhealthy") {
		return false, false
	}
	manifest, err := store.ReadManifest(instance.RunID)
	if err != nil {
		e.fail("OWNERSHIP_UNKNOWN", "cannot compare active run configuration: "+err.Error(), "inspect the run state before retrying a mutation")
		e.Errors[len(e.Errors)-1].InstanceID = instance.ID
		return false, false
	}
	changed := manifest.Digest != currentDigest
	return changed, changed
}

func addDependencyDetails(store *state.Store, project state.Project, worktree state.Worktree, service *config.Service, instance state.Instance, registered bool, info *instanceInfo, e *envelope) {
	desired := copyBindings(instance.DesiredBindings)
	effective := copyBindings(instance.EffectiveBindings)
	live := !info.StateUnknown && (info.State == "starting" || info.State == "running" || info.State == "ready" || info.State == "unhealthy")
	var manifest *state.Manifest
	if live && instance.RunID != "" {
		m, err := store.ReadManifest(instance.RunID)
		if err != nil {
			addDependencyError(info, e, manifestErrorCode(err), "cannot read active dependency bindings: "+err.Error())
		} else if m.InstanceID != instance.ID {
			addDependencyError(info, e, "STATE_CORRUPT", "active manifest instance does not match "+instance.ID)
		} else {
			manifest = &m
			effective = copyBindings(m.EffectiveBindings)
		}
	}

	dependencies := []string{}
	if service != nil {
		dependencies = append(dependencies, service.DependsOn...)
	} else {
		for dependency := range desired {
			dependencies = append(dependencies, dependency)
		}
		for dependency := range effective {
			if !contains(dependencies, dependency) {
				dependencies = append(dependencies, dependency)
			}
		}
	}
	sort.Strings(dependencies)
	seen := make(map[string]bool, len(dependencies))
	for _, dependency := range dependencies {
		seen[dependency] = true
		defaultTarget := project.ID + "/" + worktree.ID + "/" + dependency
		desiredTarget := desired[dependency]
		if desiredTarget == "" {
			desiredTarget = defaultTarget
			desired[dependency] = desiredTarget
		}
		effectiveTarget := effective[dependency]
		if effectiveTarget == "" && live {
			effectiveTarget = defaultTarget
			effective[dependency] = effectiveTarget
		}
		if live && manifest != nil && effectiveTarget != "" && effectiveTarget != desiredTarget {
			info.RestartRequired = true
		}
		if targetErr := validateDependencyTarget(project, desiredTarget, dependency, registered); targetErr != nil {
			addDependencyError(info, e, "BINDING_INVALID", targetErr.Error())
		}
		if effectiveTarget != "" {
			_, targetInstance, targetErr := dependencyTarget(project, effectiveTarget, dependency, registered)
			if targetErr != nil {
				addDependencyError(info, e, "BINDING_INVALID", targetErr.Error())
			} else if live && manifest != nil {
				if targetInstance.RunID == "" {
					addDependencyError(info, e, "OWNERSHIP_UNKNOWN", "effective dependency has no run manifest: "+effectiveTarget)
				} else if targetManifest, err := store.ReadManifest(targetInstance.RunID); err != nil {
					addDependencyError(info, e, manifestErrorCode(err), "cannot read effective dependency manifest: "+err.Error())
				} else if targetManifest.InstanceID != effectiveTarget {
					addDependencyError(info, e, "STATE_CORRUPT", "dependency manifest instance does not match "+effectiveTarget)
				} else if dependencyAddressesChanged(dependency, manifest.StopContract.Env, targetManifest.Addresses) {
					info.RestartRequired = true
				}
			}
		}
	}
	for dependency := range desired {
		if !seen[dependency] {
			addDependencyError(info, e, "BINDING_INVALID", "saved binding names an undeclared dependency: "+dependency)
		}
	}
	if len(desired) != 0 {
		info.DesiredBindings = desired
	}
	if len(effective) != 0 {
		info.EffectiveBindings = effective
	}
}

func copyBindings(bindings map[string]string) map[string]string {
	copy := make(map[string]string, len(bindings))
	for key, value := range bindings {
		copy[key] = value
	}
	return copy
}

func manifestErrorCode(err error) string {
	if errors.Is(err, state.ErrCorrupt) {
		return "STATE_CORRUPT"
	}
	return "OWNERSHIP_UNKNOWN"
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func validateDependencyTarget(project state.Project, targetID, dependency string, registered bool) error {
	_, _, err := dependencyTarget(project, targetID, dependency, registered)
	return err
}

func dependencyTarget(project state.Project, targetID, dependency string, registered bool) (state.Worktree, state.Instance, error) {
	parts := strings.Split(targetID, "/")
	if len(parts) != 3 || parts[0] != project.ID || parts[2] != dependency {
		return state.Worktree{}, state.Instance{}, fmt.Errorf("dependency %s has invalid target %s", dependency, targetID)
	}
	worktree, ok := findWorktree(project, parts[1])
	if !ok || worktree.Root == "" {
		return state.Worktree{}, state.Instance{}, fmt.Errorf("dependency %s target worktree is missing: %s", dependency, targetID)
	}
	cfg, err := config.Load(filepath.Join(worktree.Root, "dev.toml"))
	if err != nil {
		return state.Worktree{}, state.Instance{}, fmt.Errorf("dependency %s target config is invalid: %s", dependency, targetID)
	}
	if _, ok := cfg.Services[dependency]; !ok {
		return state.Worktree{}, state.Instance{}, fmt.Errorf("dependency %s target service is missing: %s", dependency, targetID)
	}
	instanceID := targetID
	instance, ok := project.Instances[instanceID]
	if registered && !ok {
		return state.Worktree{}, state.Instance{}, fmt.Errorf("dependency %s target instance is missing: %s", dependency, targetID)
	}
	return worktree, instance, nil
}

func dependencyAddressesChanged(dependency string, previous, current map[string]string) bool {
	normalized := strings.ToUpper(strings.ReplaceAll(dependency, "-", "_"))
	prefix := "RDR_DEP_" + normalized + "_"
	for _, field := range []string{"HOST", "PORT", "DIRECT_URL", "URL"} {
		old, exists := previous[prefix+field]
		if !exists {
			continue
		}
		if current[strings.ToLower(field)] != old {
			return true
		}
	}
	return false
}

func addDependencyError(info *instanceInfo, e *envelope, code, message string) {
	if info.DependencyError == "" {
		info.DependencyError = message
	} else if !strings.Contains(info.DependencyError, message) {
		info.DependencyError += "; " + message
	}
	for _, existing := range e.Errors {
		if existing.InstanceID == info.ID && existing.Message == message {
			return
		}
	}
	e.fail(code, message)
	e.Errors[len(e.Errors)-1].InstanceID = info.ID
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
