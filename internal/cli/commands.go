// Dispatches rundevrun commands and assembles their results.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/bettertomorrow-dev/rundevrun/internal/diagnostics"
	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/localrouting"
	"github.com/bettertomorrow-dev/rundevrun/internal/monitor"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/proxy"
	"github.com/bettertomorrow-dev/rundevrun/internal/publication"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

type projectInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Root string `json:"root"`
}

type openResult struct {
	URL string `json:"url"`
}

func (r openResult) String() string { return r.URL }

type publicationResult struct {
	publication.Status
	Password string `json:"-"`
}

func (r publicationResult) String() string {
	line := fmt.Sprintf("%s\t%s", r.State, r.URL)
	if r.Password != "" {
		line = fmt.Sprintf("%s\nPassword: %s", r.URL, r.Password)
	}
	if r.ErrorCode != "" {
		line += "\n" + r.ErrorCode + ": " + r.ErrorMessage
	}
	if len(r.LogPaths) > 0 {
		line += "\nLogs: " + strings.Join(r.LogPaths, ", ")
	}
	return line
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
	ConfigFile        string            `json:"config_file,omitempty"`
	ConfigMode        string            `json:"config_mode,omitempty"`
	WorktreeRoot      string            `json:"worktree_root,omitempty"`
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
	RouteChanged      bool              `json:"route_changed,omitempty"`
	LogPaths          map[string]string `json:"log_paths,omitempty"`
	projectName       string
	worktreeLabel     string
	displayURLs       []string
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
	if opts.noProxy && e.Command != "up" && e.Command != "restart" && e.Command != "switch" {
		e.fail("USAGE", "--no-proxy is only valid with up, restart, or switch")
		return
	}
	if (opts.mode != "" || opts.recreate) && e.Command != "share" {
		e.fail("USAGE", "--mode and --recreate are only valid with share")
		return
	}
	if opts.toWorktree != "" && e.Command != "bind" && e.Command != "switch" {
		e.fail("USAGE", "--to-worktree is only supported by bind or switch")
		return
	}
	if opts.allServices && e.Command != "switch" {
		e.fail("USAGE", "--all-services is only valid with switch")
		return
	}
	if opts.file != "" && e.Command != "switch" && e.Command != "config external" && !strings.HasPrefix(e.Command, "network ") {
		e.fail("USAGE", "--file is only valid with switch, config external, or network commands")
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
	if e.Command == "version" {
		if validArity(args, 1, 1, e) {
			e.Results = append(e.Results, versionInfo{"rundevrun", Version, APIVersion})
		}
		return
	}
	root, err := runtimeRoot()
	if err != nil {
		failError(e, err)
		return
	}
	store := &state.Store{Root: root}
	switch e.Command {
	case "config external":
		externalConfigCommand(args[2:], opts, e)
	case "network inspect", "network check", "network plan", "network apply", "network setup-plan", "network setup-apply", "network setup-check", "network credential-store", "network setup-recover", "network adopt-dns", "network adopt-serve", "network resolve-challenge", "network credential-replace", "network adopt-existing-dns", "network cleanup-plan", "network cleanup-apply", "network resolve-finalization":
		networkingCommand(ctx, store, args, opts, e)
	case "proxy status", "proxy reload", "proxy stop", "proxy abort":
		proxyCommand(ctx, store, args[1:], opts, e)
	case "alias":
		aliasCommand(ctx, store, args[1:], opts, e)
	case "open":
		openCommand(ctx, store, args[1:], opts, e)
	case "doctor":
		doctorCommand(ctx, store, args[1:], opts, e)
	case "share", "unshare":
		publicationCommand(ctx, store, args[1:], opts, e)
	case "switch":
		switchCommand(ctx, store, args[1:], opts, e)
	case "bind":
		if !validArity(args, 3, 3, e) {
			return
		}
		if opts.toWorktree == "" {
			e.fail("USAGE", "bind requires --to-worktree", "see rundevrun --help")
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
	case "projects", "status":
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
		listRegistry(ctx, store, opts, e)
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
		listConfiguration(ctx, store, opts, e)
	default:
		e.fail("USAGE", "unknown or not yet implemented command: "+e.Command, "see rundevrun --help")
	}
	if e.text && (opts.dryRun || e.Command == "bind" || e.Command == "switch" || e.Command == "alias" || e.Command == "network plan") {
		if reg, err := store.Read(); err == nil {
			for id := range reg.Projects {
				if p, err := reg.Project(id); err == nil {
					for _, w := range p.Worktrees {
						e.rememberWorktree(id, p.Name, w.ID, w.Branch, w.Root)
					}
				}
			}
		}
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
	cfg, cfgPath, cfgErr := machineRouting()
	if cfgErr != nil {
		failError(e, cfgErr)
		return
	}
	r.Routing = &cfg
	if e.text {
		for _, mapping := range mappings {
			from, to := mapping.From, mapping.To
			e.rememberWorktree(from.Project.ID, from.Project.Name, from.Worktree.ID, from.Worktree.Branch, from.Worktree.Root)
			e.rememberWorktree(to.Project.ID, to.Project.Name, to.Worktree.ID, to.Worktree.Branch, to.Worktree.Root)
			e.switchMappings = append(e.switchMappings, runner.SwitchPair{
				From: from.Project.ID + "/" + from.Worktree.ID + "/" + from.Service,
				To:   to.Project.ID + "/" + to.Worktree.ID + "/" + to.Service,
			})
		}
	}
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
	started := false
	for _, item := range out.Results {
		if item.Status == "completed" && !item.StateUnknown && liveState(item.State) {
			started = true
			break
		}
	}
	if started && cfg.Routing.Enabled {
		needs, checkErr := graphNeedsProxy(store, out.Results)
		if checkErr != nil {
			appendProxyError(ctx, e, checkErr)
		} else if needs {
			ensureProxyAfterStart(ctx, store, cfgPath, e)
		}
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

func machineRouting() (routing.Config, string, error) {
	path, err := routing.DefaultPath()
	if err != nil {
		return routing.Config{}, "", err
	}
	cfg, err := localrouting.Load(path)
	return cfg, path, err
}

func proxyView(info proxy.Info) *proxyInfoView {
	return &proxyInfoView{State: info.State, PID: info.Identity.PID, HTTPListen: info.HTTPListen, HTTPSListen: info.HTTPSListen, TLSMode: info.TLSMode, ConfigPath: info.ConfigPath, PreparedListen: info.PreparedListen, TLSError: info.TLSError, RenewalError: info.RenewalError, CertificateActivationPending: info.CertificateActivationPending}
}

func openCommand(ctx context.Context, store *state.Store, args []string, opts options, e *envelope) {
	if opts.dryRun || opts.toWorktree != "" || opts.file != "" || opts.all || opts.cascade || opts.retryHooks || opts.allServices || opts.noProxy || opts.follow || opts.tailSet {
		e.fail("USAGE", "open accepts a service and project/worktree selectors only")
		return
	}
	if !validArity(append([]string{"open"}, args...), 2, 2, e) {
		return
	}
	if ctx.Err() != nil {
		failError(e, ctx.Err())
		return
	}
	d, w := resolveCommandSelection(store, opts, false, e)
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
	id := d.ID + "/" + w.ID + "/" + args[0]
	instance, ok := p.Instances[id]
	if !ok {
		e.fail("NOT_FOUND", "registered service instance not found: "+args[0])
		return
	}
	openURL := localEntryURL(instance)
	if openURL == "" {
		observed, err := store.Inspect(instance)
		if err == nil && liveState(observed) && instance.RunID != "" {
			manifest, readErr := store.ReadManifest(instance.RunID)
			if readErr == nil && manifest.InstanceID == id && manifest.Launch.Protocol == "http" {
				if port, verifyErr := runner.VerifyProxyTarget(store, d.ID, id, instance.RunID); verifyErr == nil {
					candidate := manifest.Addresses["direct_url"]
					if parsed, parseErr := url.Parse(candidate); parseErr == nil && parsed.Scheme == "http" && parsed.Host != "" && parsed.Port() == strconv.Itoa(port) {
						openURL = candidate
					}
				}
			}
		}
	}
	if openURL != "" && instance.RunID != "" {
		reported, readErr := runner.New(store, "").ReportedURL(ctx, instance.RunID)
		if readErr == nil {
			openURL, _ = runner.TransferOwnedReportedURL(store, d.ID, instance, openURL, reported)
		}
	}
	if openURL == "" {
		e.fail("NOT_FOUND", "service has no registered browser route or active HTTP URL: "+id)
		return
	}
	e.Results = append(e.Results, openResult{URL: openURL})
	if !opts.json {
		if err := openPlatformURL(ctx, openURL); err != nil {
			failError(e, err)
		}
	}
}

func publicationCommand(ctx context.Context, store *state.Store, args []string, opts options, e *envelope) {
	if !validArity(append([]string{e.Command}, args...), 2, 2, e) {
		return
	}
	if opts.mode != "" && opts.mode != "live" && opts.mode != "snapshot" {
		e.fail("USAGE", "--mode must be live or snapshot")
		return
	}
	if ctx.Err() != nil {
		failError(e, ctx.Err())
		return
	}
	d, w := resolveCommandSelection(store, opts, false, e)
	if d == nil {
		return
	}
	selection := runner.Selection{Project: d, Worktree: w, Service: args[0]}
	manager := &publication.Manager{Store: store, Executable: executablePath()}
	if e.Command == "share" {
		if opts.dryRun {
			plan, err := manager.Plan(ctx, selection, publication.Options{Mode: opts.mode})
			if err != nil {
				failError(e, err)
				return
			}
			e.Plan = struct {
				Action string           `json:"action"`
				Plan   publication.Plan `json:"plan"`
			}{"share", plan}
			return
		}
		created, err := manager.Share(ctx, selection, publication.Options{Mode: opts.mode, Recreate: opts.recreate})
		if err != nil {
			failShare(e, created, err)
			return
		}
		if created.Password != "" {
			e.Credentials = &shareCredentials{Password: created.Password}
		}
		e.Results = append(e.Results, publicationResult{Status: created.Status, Password: created.Password})
		return
	}
	if opts.dryRun {
		current, err := manager.Status(ctx, selection)
		if err != nil {
			failError(e, err)
			return
		}
		e.Plan = struct {
			Action string             `json:"action"`
			State  publication.Status `json:"publication"`
		}{"unshare", current}
		return
	}
	result, err := manager.Unshare(ctx, selection)
	if err != nil {
		failError(e, err)
		return
	}
	e.Results = append(e.Results, result.Status)
}

func failShare(e *envelope, result publication.Result, err error) {
	e.Credentials = nil
	if result.ID != "" {
		e.Results = append(e.Results, publicationResult{Status: result.Status})
		failError(e, err)
		if result.State == "ready" {
			e.Errors[len(e.Errors)-1].Actions = []string{"inspect status, then unshare and create a fresh share to obtain new credentials"}
			e.Status = "partial"
		} else {
			e.Errors[len(e.Errors)-1].Actions = []string{"inspect status and the listed publication logs"}
		}
		return
	}
	failError(e, err)
}

func openPlatformURL(ctx context.Context, url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", url)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

type doctorResult struct {
	ProjectID string             `json:"project_id,omitempty"`
	Report    diagnostics.Report `json:"report"`
}

func (r doctorResult) String() string {
	var b strings.Builder
	if r.ProjectID != "" {
		fmt.Fprintf(&b, "Project %s\n", r.ProjectID)
	}
	for _, p := range r.Report.Projects {
		fmt.Fprintf(&b, "Registry %s: %s %s\n", p.ID, p.Registry.Status, p.Registry.Message)
	}
	fmt.Fprintf(&b, "Configuration: %s %s\nProxy: %s %s\nHTTP listener: %s %s\n", r.Report.Configuration.Status, r.Report.Configuration.Message, r.Report.Proxy.Status.Status, r.Report.Proxy.Status.Message, r.Report.Proxy.HTTP.Status, r.Report.Proxy.HTTP.Message)
	for _, route := range r.Report.Routes {
		fmt.Fprintf(&b, "%s %s (%s)\n  DNS: %s %s\n  TLS system: %s %s\n  TLS configured: %s %s\n  Ownership: %s %s\n  Application: %s %s\n", route.Service, route.InstanceID, route.Hostname, route.DNS.Status, route.DNS.Message, route.TLSSystem.Status, route.TLSSystem.Message, route.TLSConfigured.Status, route.TLSConfigured.Message, route.Ownership.Status, route.Ownership.Message, route.Application.Status, route.Application.Message)
	}
	return strings.TrimSpace(b.String())
}

func doctorCommand(ctx context.Context, store *state.Store, args []string, opts options, e *envelope) {
	if len(args) != 0 || opts.dryRun || opts.toWorktree != "" || opts.file != "" || opts.all || opts.cascade || opts.retryHooks || opts.allServices || opts.noProxy || opts.follow || opts.tailSet {
		e.fail("USAGE", "doctor accepts optional --project and --worktree selectors only")
		return
	}
	if opts.worktree != "" && opts.project == "" {
		e.fail("USAGE", "doctor --worktree requires --project")
		return
	}
	path, err := routing.DefaultPath()
	if err != nil {
		failError(e, err)
		return
	}
	projectID := ""
	if opts.project != "" {
		reg, err := store.Read()
		if err != nil {
			failError(e, err)
			return
		}
		p := selectProject(reg, opts.project, e)
		if p == nil {
			return
		}
		projectID = p.ID
		e.Scope.Project = p.ID
	}
	worktreeID := ""
	if opts.worktree != "" {
		reg, err := store.Read()
		if err != nil {
			failError(e, err)
			return
		}
		p, err := reg.Project(projectID)
		if err != nil {
			failError(e, err)
			return
		}
		w, err := discovery.ResolveDescriptor(descriptor(p), opts.worktree)
		if err != nil {
			failError(e, err)
			return
		}
		worktreeID = w.ID
		e.Scope.Worktree = w.ID
	}
	if ctx.Err() != nil {
		failError(e, ctx.Err())
		return
	}
	report, err := diagnostics.Run(ctx, store, path, projectID, worktreeID)
	if err != nil {
		failError(e, err)
		return
	}
	e.Results = append(e.Results, doctorResult{ProjectID: projectID, Report: report})
	if reportFailed(report) {
		e.fail("DIAGNOSTICS_FAILED", "one or more diagnostic checks failed")
	}
	if ctx.Err() != nil {
		for _, failure := range e.Errors {
			if failure.Code == "CANCELLED" {
				return
			}
		}
		failError(e, ctx.Err())
	}
}

func reportFailed(r diagnostics.Report) bool {
	for _, project := range r.Projects {
		if project.Registry.Status == "fail" {
			return true
		}
	}
	if r.Configuration.Status == "fail" || (r.LegacyTLS != nil && r.LegacyTLS.Status == "fail") || r.Proxy.Status.Status == "fail" || r.Proxy.HTTP.Status == "fail" {
		return true
	}
	for _, route := range r.Routes {
		if route.DNS.Status == "fail" || route.TLSSystem.Status == "fail" || route.TLSConfigured.Status == "fail" || route.Ownership.Status == "fail" || route.Application.Status == "fail" {
			return true
		}
	}
	return false
}

func proxyCommand(ctx context.Context, store *state.Store, args []string, opts options, e *envelope) {
	if opts.project != "" || opts.worktree != "" || opts.toWorktree != "" || opts.file != "" || opts.all || opts.cascade || opts.retryHooks || opts.allServices || opts.noProxy || opts.follow || opts.tailSet {
		e.fail("USAGE", "proxy commands do not accept selectors or unrelated flags")
		return
	}
	if len(args) != 1 {
		e.fail("USAGE", "proxy requires status, reload, or stop")
		return
	}
	switch args[0] {
	case "status":
		if opts.dryRun {
			e.fail("USAGE", "proxy status does not support --dry-run")
			return
		}
		info, err := proxy.Status(store)
		if err != nil {
			failError(e, err)
			return
		}
		e.Proxy = proxyView(info)
	case "reload":
		if opts.dryRun {
			info, err := proxy.Status(store)
			if err != nil {
				failError(e, err)
				return
			}
			if info.State != "ready" {
				failError(e, &proxy.Error{Code: "NOT_FOUND", Message: "proxy is not running"})
				return
			}
			cfg, err := proxy.ValidateConfiguration(store, info.ConfigPath)
			if err != nil {
				failError(e, err)
				return
			}
			expectedHTTPS := cfg.Routing.HTTPSListen
			expectedTLS := "provided"
			if cfg.ExternalTLS() {
				expectedHTTPS = ""
				expectedTLS = "external"
			}
			candidate := map[string]any{"http_listen": cfg.Routing.HTTPListen, "https_listen": expectedHTTPS, "tls_mode": expectedTLS, "config_path": info.ConfigPath}
			e.Plan = map[string]any{"action": "proxy reload", "proxy": proxyView(info), "candidate": candidate}
			if err := proxyReloadCompatibility(info, cfg); err != nil {
				failError(e, err)
				return
			}
			return
		}
		cfg, _, cfgErr := machineRouting()
		if cfgErr != nil {
			failError(e, cfgErr)
			return
		}
		if cfg.LocalOnly {
			if err := proxy.PrepareReload(ctx, store); err != nil {
				failError(e, err)
				return
			}
			defer proxy.AbortReload(context.Background(), store)
			if err := runner.ReconcileLocalRoutes(ctx, store, cfg); err != nil {
				failError(e, err)
				return
			}
		}
		if err := proxy.Reload(ctx, store); err != nil {
			failError(e, err)
			return
		}
		info, err := proxy.Status(store)
		if err != nil {
			failError(e, err)
			return
		}
		e.Proxy = proxyView(info)
	case "abort":
		if opts.dryRun {
			e.fail("USAGE", "proxy abort does not support --dry-run")
			return
		}
		if err := proxy.AbortReload(ctx, store); err != nil {
			failError(e, err)
			return
		}
		info, err := proxy.Status(store)
		if err != nil {
			failError(e, err)
			return
		}
		e.Proxy = proxyView(info)
	case "stop":
		if opts.dryRun {
			info, err := proxy.Status(store)
			if err != nil {
				failError(e, err)
				return
			}
			e.Plan = map[string]any{"action": "proxy stop", "proxy": proxyView(info)}
			return
		}
		if err := proxy.Stop(ctx, store); err != nil {
			failError(e, err)
			return
		}
		info, err := proxy.Status(store)
		if err != nil {
			failError(e, err)
			return
		}
		e.Proxy = proxyView(info)
	default:
		e.fail("USAGE", "unknown proxy command: "+args[0])
	}
}

func proxyReloadCompatibility(info proxy.Info, cfg routing.Config) error {
	if cfg.LocalOnly && (info.TLSMode == "local" || info.TLSMode == "managed") {
		return nil
	}
	expectedHTTPS := cfg.Routing.HTTPSListen
	if cfg.ExternalTLS() {
		expectedHTTPS = ""
	}
	expectedTLS := "provided"
	if cfg.ExternalTLS() {
		expectedTLS = "external"
	}
	if info.HTTPListen != cfg.Routing.HTTPListen || info.HTTPSListen != expectedHTTPS || info.TLSMode != expectedTLS {
		return &proxy.Error{Code: "CONFIG_INVALID", Message: "proxy listener or TLS mode change requires restart"}
	}
	return nil
}

func aliasCommand(ctx context.Context, store *state.Store, args []string, opts options, e *envelope) {
	if opts.toWorktree != "" || opts.file != "" || opts.all || opts.cascade || opts.retryHooks || opts.allServices || opts.noProxy || opts.follow || opts.tailSet {
		e.fail("USAGE", "alias does not accept lifecycle or file flags")
		return
	}
	if len(args) < 1 || len(args) > 2 {
		e.fail("USAGE", "alias requires LABEL or SERVICE FULL_HOSTNAME")
		return
	}
	d, w := resolveCommandSelection(store, opts, true, e)
	if d == nil {
		return
	}
	cfg, _, err := machineRouting()
	if err != nil {
		failError(e, err)
		return
	}
	r := runner.New(store, executablePath())
	r.Routing = &cfg
	var result any
	if len(args) == 1 {
		if opts.dryRun {
			result, err = r.PlanAlias(ctx, d, w, args[0])
		} else {
			result, err = r.SetAlias(ctx, d, w, args[0])
		}
	} else {
		selection := runner.Selection{Project: d, Worktree: w, Service: args[0]}
		if opts.dryRun {
			result, err = r.PlanHostnameAlias(ctx, selection, args[1])
		} else {
			result, err = r.SetHostnameAlias(ctx, selection, args[1])
		}
	}
	if err != nil {
		failError(e, err)
		return
	}
	if opts.dryRun {
		e.Plan = result
		return
	}
	e.Results = append(e.Results, result)
	refreshProxyAfterAlias(ctx, store, e)
}

func refreshProxyAfterAlias(ctx context.Context, store *state.Store, e *envelope) {
	info, statusErr := proxy.Status(store)
	if ctx.Err() != nil {
		appendProxyError(ctx, e, ctx.Err())
		return
	}
	if statusErr != nil {
		appendProxyError(ctx, e, statusErr)
		return
	}
	e.Proxy = proxyView(info)
	if info.State == "ready" {
		if err := proxy.Reload(ctx, store); err != nil {
			appendProxyError(ctx, e, err)
			return
		}
		info, err := proxy.Status(store)
		if err != nil {
			appendProxyError(ctx, e, err)
			return
		}
		e.Proxy = proxyView(info)
	}
}

func appendProxyError(ctx context.Context, e *envelope, err error) {
	code := proxyErrorCode(ctx, err)
	e.ProxyErrors = append(e.ProxyErrors, commandError{Code: code, Message: err.Error(), Actions: []string{}})
	if code == "CANCELLED" {
		e.fail("CANCELLED", err.Error())
		return
	}
	e.Status = "partial"
}

func appendRoutingError(err error, e *envelope) {
	code := "IO_ERROR"
	var re *routing.Error
	var pe *proxy.Error
	var runErr *runner.Error
	switch {
	case errors.As(err, &re):
		code = re.Code
	case errors.As(err, &pe):
		code = pe.Code
	case errors.As(err, &runErr):
		code = runErr.Code
	case errors.Is(err, state.ErrCorrupt):
		code = "STATE_CORRUPT"
	}
	e.RoutingErrors = append(e.RoutingErrors, commandError{Code: code, Message: err.Error(), Actions: []string{}})
	e.Status = "partial"
}

func proxyErrorCode(ctx context.Context, err error) string {
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		return "CANCELLED"
	}
	var pe *proxy.Error
	if errors.As(err, &pe) {
		return pe.Code
	}
	var re *routing.Error
	if errors.As(err, &re) {
		return re.Code
	}
	if errors.Is(err, state.ErrOwnershipUnknown) {
		return "OWNERSHIP_UNKNOWN"
	}
	if errors.Is(err, state.ErrCorrupt) {
		return "STATE_CORRUPT"
	}
	return "IO_ERROR"
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
		e.fail("USAGE", "missing service selector", "see rundevrun --help")
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
		runGraph(ctx, store, "down", selections, opts.cascade, opts.retryHooks, opts.noProxy, e)
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
		runGraph(ctx, store, "down", selections, opts.cascade, opts.retryHooks, opts.noProxy, e)
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
	var d *discovery.Project
	var w *discovery.Worktree
	var candidate *state.Registry
	if e.Command == "up" {
		d, w, candidate = discoverForUp(store, opts, e)
	} else {
		d, w = resolveCommandSelection(store, opts, requireConfig, e)
	}
	if d == nil {
		return
	}
	reg, err := store.Read()
	if err != nil {
		failError(e, err)
		return
	}
	if candidate != nil {
		reg = *candidate
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
	for _, tree := range d.Worktrees {
		e.rememberWorktree(d.ID, d.Name, tree.ID, tree.Branch, tree.Root)
	}
	if opts.dryRun {
		if candidate != nil {
			r := runner.New(store, executablePath())
			cfg, _, err := machineRouting()
			if err != nil {
				failError(e, err)
				return
			}
			r.Routing = &cfg
			plan, err := r.PlanGraphWithRegistry(ctx, e.Command, selections, opts.cascade, reg)
			if err != nil {
				failError(e, err)
				return
			}
			e.Plan = plan
			return
		}
		setGraphPlan(ctx, store, e.Command, selections, opts.cascade, reg.Revision, e)
		return
	}
	started := time.Now().UTC()
	graph := runGraph(ctx, store, e.Command, selections, opts.cascade, opts.retryHooks, opts.noProxy, e)
	if e.Command == "up" || e.Command == "restart" {
		attempt := fmt.Sprintf("%d-%d", started.UnixNano(), os.Getpid())
		outcomes := map[string]string{}
		for _, item := range graph.Results {
			if item.InstanceID == "" {
				continue
			}
			switch item.Status {
			case "completed", "unchanged":
				outcomes[item.InstanceID] = ""
			case "not_run":
				outcomes[item.InstanceID] = "Not started because another service failed"
			case "failed":
				outcomes[item.InstanceID] = "Service launch failed"
			}
		}
		for _, failure := range graph.Errors {
			if failure.InstanceID != "" {
				outcomes[failure.InstanceID] = failure.Code + ": " + failure.Message
			}
		}
		for _, sel := range selections {
			id := sel.Project.ID + "/" + sel.Worktree.ID + "/" + sel.Service
			if _, ok := outcomes[id]; !ok {
				message := ""
				for _, failure := range graph.Errors {
					if failure.InstanceID == "" {
						message = failure.Code + ": " + failure.Message
						break
					}
				}
				if message == "" && graph.Status != "ok" {
					message = "Service was not started"
				}
				if message == "" && graph.Status == "" && len(e.Errors) > 0 {
					message = e.Errors[0].Code + ": " + e.Errors[0].Message
				}
				outcomes[id] = message
			}
		}
		results := make([]state.LaunchResult, 0, len(outcomes))
		for id, message := range outcomes {
			parts := strings.Split(id, "/")
			if len(parts) == 3 {
				results = append(results, state.LaunchResult{Attempt: attempt, ProjectID: parts[0], InstanceID: id, StartedAt: started, Error: message})
			}
		}
		if err := store.RecordLaunch(results); err != nil {
			failError(e, err)
		}
	}
}

func discoverForUp(store *state.Store, opts options, e *envelope) (*discovery.Project, *discovery.Worktree, *state.Registry) {
	cwd, err := os.Getwd()
	if err != nil {
		failError(e, err)
		return nil, nil, nil
	}
	var p *discovery.Project
	if opts.project == "" {
		p, err = discovery.Discover(cwd)
	} else {
		reg, readErr := store.Read()
		if readErr != nil {
			failError(e, readErr)
			return nil, nil, nil
		}
		saved := selectProject(reg, opts.project, e)
		if saved == nil {
			return nil, nil, nil
		}
		roots := []string{cwd, saved.Root}
		for _, old := range saved.Worktrees {
			roots = append(roots, old.Root)
		}
		for _, root := range roots {
			fresh, discoverErr := discovery.Discover(root)
			if discoverErr == nil && fresh.ID == saved.ID {
				p = fresh
				break
			}
			if discoverErr != nil {
				err = discoverErr
			}
		}
		if p == nil && err == nil {
			err = errors.New("registered project could not be discovered")
		}
	}
	if err != nil && p == nil {
		failError(e, err)
		return nil, nil, nil
	}
	selector := opts.worktree
	if selector == "" {
		canonical, err := filepath.EvalSymlinks(cwd)
		if err != nil {
			failError(e, err)
			return nil, nil, nil
		}
		for _, candidate := range p.Worktrees {
			if within(canonical, candidate.Root) && len(candidate.Root) > len(selector) {
				selector = candidate.Root
			}
		}
	}
	if selector == "" {
		e.fail("USAGE", "outside the selected project, specify --worktree")
		return nil, nil, nil
	}
	w, err := discovery.Resolve(p, selector)
	if err != nil {
		failError(e, err)
		return nil, nil, nil
	}
	e.Scope = scope{Project: p.ID, Worktree: w.ID}
	merge := func(reg *state.Registry) error {
		saved, err := reg.Project(p.ID)
		alreadyRegistered := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		mergeDiscoveredProject(&saved, p)
		if err := reg.SetProject(p.ID, saved); err != nil {
			return err
		}
		cfg, _, err := machineRouting()
		if err != nil {
			if alreadyRegistered {
				return err
			}

			return nil
		}
		if cfg.Routing.Enabled {
			r := runner.New(store, executablePath())
			r.Routing = &cfg
			return r.AssignProjectRoutes(reg, p)
		}
		return nil
	}
	if opts.dryRun {
		reg, err := store.Read()
		if err == nil {
			err = merge(&reg)
		}
		if err != nil {
			failError(e, err)
			return nil, nil, nil
		}
		return p, w, &reg
	}
	if err := store.Update(merge); err != nil {
		failError(e, err)
		return nil, nil, nil
	}
	return p, w, nil
}

func setGraphPlan(ctx context.Context, store *state.Store, action string, selections []runner.Selection, cascade bool, revision uint64, e *envelope) {
	if len(selections) == 0 {
		e.Plan = runner.GraphPlan{Action: action, Revision: revision, Instances: []runner.PlannedInstance{}}
		return
	}
	r := runner.New(store, executablePath())
	if action == "up" || action == "restart" {
		cfg, _, err := machineRouting()
		if err != nil {
			failError(e, err)
			return
		}
		r.Routing = &cfg
	}
	plan, err := r.PlanGraph(ctx, action, selections, cascade)
	if err != nil {
		failError(e, err)
		return
	}
	e.Plan = plan
}

func runGraph(ctx context.Context, store *state.Store, action string, selections []runner.Selection, cascade, retryHooks, noProxy bool, e *envelope) runner.GraphResult {
	r := runner.New(store, executablePath())
	var routeCfg *routing.Config
	var routePath string
	if action == "up" || action == "restart" {
		cfg, path, err := machineRouting()
		if err != nil {
			failError(e, err)
			return runner.GraphResult{}
		}
		routePath = path
		routeCfg = &cfg
		r.Routing = routeCfg
	}
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
		return runner.GraphResult{}
	}
	for _, item := range result.Results {
		if action == "up" {
			e.Results = append(e.Results, graphInstanceInfo(store, item, e))
		} else {
			e.Results = append(e.Results, item)
		}
	}
	e.OperationID = result.OperationID
	e.JournalPath = result.JournalPath
	for _, step := range result.Steps {
		e.Steps = append(e.Steps, operationStepView{ID: step.ID, Phase: step.Status, Status: terminalStepStatus(step.Status), Code: step.Code, Message: step.Message})
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
	if (action == "up" || action == "restart") && hasSuccessfulLiveResult(result.Results) {
		if routeCfg != nil && routeCfg.Routing.Enabled {
			needs, checkErr := graphNeedsProxy(store, result.Results)
			if checkErr != nil {
				appendProxyError(ctx, e, checkErr)
			} else if needs {
				ensureProxyAfterStart(ctx, store, routePath, e)
			}
		}
		if e.lifecycleURLs != nil {
			collectLifecycleURLs(ctx, store, result.Results, e.lifecycleURLs)
		}
	}
	return result
}

func collectLifecycleURLs(ctx context.Context, store *state.Store, results []runner.Result, urls map[string][]string) {
	reg, err := store.Read()
	if err != nil {
		return
	}
	for _, item := range results {
		if !successfulLiveResult(item) {
			continue
		}
		projectID, _, ok := strings.Cut(item.InstanceID, "/")
		if !ok {
			continue
		}
		project, err := reg.Project(projectID)
		if err != nil {
			continue
		}
		instance, ok := project.Instances[item.InstanceID]
		if !ok || instance.RunID == "" || (item.RunID != "" && instance.RunID != item.RunID) {
			continue
		}
		urls[item.InstanceID] = serviceDisplayURLs(ctx, store, projectID, instance)
	}
}

func (i *instanceInfo) setDisplayURLs(ctx context.Context, store *state.Store, instance state.Instance) {
	if i.StateUnknown || !liveState(i.State) {
		return
	}
	i.displayURLs = serviceDisplayURLs(ctx, store, i.ProjectID, instance)
	if len(i.displayURLs) == 0 && i.URL != "" {
		i.displayURLs = []string{i.URL}
	}
}

func serviceDisplayURLs(ctx context.Context, store *state.Store, projectID string, instance state.Instance) []string {
	if instance.RunID == "" {
		return nil
	}
	manifest, err := store.ReadManifest(instance.RunID)
	if err != nil || manifest.InstanceID != instance.ID {
		return nil
	}
	view := monitor.Service{ProjectID: projectID, RunID: instance.RunID}
	_ = view.LoadURLs(ctx, store, instance, manifest)
	local, reported := view.LocalURL, view.ReportedURL
	if local == "" {
		local = instance.RouteURL
		if local == "" {
			local = view.DirectURL
		}
		if url, ok := runner.TransferOwnedReportedURL(store, projectID, instance, local, reported); ok {
			local, reported = url, ""
		}
	}
	return []string{local, view.TailnetURL, view.LocalAliasURL, view.TailnetAliasURL, reported}
}

func ensureProxyAfterStart(ctx context.Context, store *state.Store, path string, e *envelope) {
	if err := proxy.Ensure(ctx, store, executablePath(), path); err != nil {
		appendProxyError(ctx, e, err)
		return
	}
	info, err := proxy.Status(store)
	if err != nil {
		appendProxyError(ctx, e, err)
		return
	}
	e.Proxy = proxyView(info)
}

func hasSuccessfulLiveResult(results []runner.Result) bool {
	for _, r := range results {
		if successfulLiveResult(r) {
			return true
		}
	}
	return false
}

func successfulLiveResult(r runner.Result) bool {
	return (r.Status == "completed" || r.Status == "unchanged") && !r.StateUnknown && liveState(r.State)
}

func graphNeedsProxy(store *state.Store, results []runner.Result) (bool, error) {
	reg, err := store.Read()
	if err != nil {
		return false, err
	}
	queue := []string{}
	seen := map[string]bool{}
	for _, result := range results {
		if !successfulLiveResult(result) {
			continue
		}
		if result.URLs["url"] != "" && result.URLs["url"] != result.URLs["direct_url"] {
			return true, nil
		}
		queue = append(queue, result.InstanceID)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		parts := strings.Split(id, "/")
		if len(parts) != 3 {
			continue
		}
		p, err := reg.Project(parts[0])
		if err != nil {
			return false, err
		}
		instance, ok := p.Instances[id]
		if !ok {
			continue
		}
		if instance.RouteURL != "" {
			return true, nil
		}
		runID := instance.RunID
		for _, result := range results {
			if result.InstanceID == id && result.RunID != "" {
				runID = result.RunID
				break
			}
		}
		if runID == "" {
			continue
		}
		manifest, err := store.ReadManifest(runID)
		if err != nil {
			return false, err
		}
		if manifest.InstanceID != id {
			return false, state.ErrCorrupt
		}
		for _, target := range manifest.EffectiveBindings {
			queue = append(queue, target)
		}
	}
	return false, nil
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
	cfg, err := discovery.LoadConfig(worktree.Root)
	if err != nil {
		fields["config_error"] = err.Error()
		return fields
	}
	service, ok := cfg.Services[instance.Service]
	if !ok {
		fields["config_error"] = "service is absent from current rundevrun.toml"
		return fields
	}
	changed, restartRequired := configDrift(store, instance, cfg.Digest, e)
	info := instanceInfo{ID: instance.ID, ProjectID: project.ID, WorktreeID: worktree.ID, Service: instance.Service, State: result.State, StateUnknown: result.StateUnknown, RunID: instance.RunID, ConfigChanged: changed, RestartRequired: restartRequired}
	addRunDetails(store, instance, &info)
	addDependencyDetails(store, project, worktree, &service, instance, true, &info, e)
	applyRouteView(store, instance, &info)
	applyRouteView(store, instance, &info)
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
	if info.URL != "" {
		fields["url"] = info.URL
	}
	if info.DirectURL != "" {
		fields["direct_url"] = info.DirectURL
	}
	if info.RouteChanged {
		fields["route_changed"] = true
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
	routeCfg, _, routeCfgErr := machineRouting()
	routeEnabled := routeCfgErr == nil && routeCfg.Routing.Enabled
	e.Results = append(e.Results, projectInfo{p.ID, p.Name, p.Root})

	for _, w := range p.Worktrees {
		info := worktreeInfo{ID: w.ID, ProjectID: p.ID, Root: w.Root, Branch: w.Branch, Detached: w.Detached, ConfigValid: w.Config != nil}
		if w.ConfigError != nil {
			info.ConfigError = w.ConfigError.Error()
			e.fail("CONFIG_INVALID", w.Root+": "+w.ConfigError.Error(), "fix rundevrun.toml in this worktree")
		}
		e.Results = append(e.Results, info)
	}
	if dryRun {
		candidate := r
		saved, readErr := candidate.Project(p.ID)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			failError(e, readErr)
			return
		}
		mergeDiscoveredProject(&saved, p)
		if err := candidate.SetProject(p.ID, saved); err != nil {
			failError(e, err)
			return
		}
		if routeCfgErr != nil {
			appendRoutingError(routeCfgErr, e)
		} else if routeEnabled {
			rr := runner.New(store, executablePath())
			rr.Routing = &routeCfg
			if err := rr.AssignProjectRoutes(&candidate, p); err != nil {
				appendRoutingError(err, e)
			}
		}
		if len(e.Errors) > 0 {
			e.Status = "partial"
		}
		return
	}
	var routeErr error
	err = store.Update(func(r *state.Registry) error {
		saved, err := r.Project(p.ID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		mergeDiscoveredProject(&saved, p)
		if err := r.SetProject(p.ID, saved); err != nil {
			return err
		}
		if routeCfgErr != nil {
			routeErr = routeCfgErr
		} else if routeEnabled {
			rr := runner.New(store, executablePath())
			rr.Routing = &routeCfg
			if routeErr = rr.AssignProjectRoutes(r, p); routeErr != nil {
				return nil
			}
		}
		return nil
	})
	if err != nil {
		e.Results = []any{}
		failError(e, err)
	} else if len(e.Errors) > 0 {
		e.Status = "partial"
	}
	if routeErr != nil {
		appendRoutingError(routeErr, e)
	}
}

func mergeDiscoveredProject(saved *state.Project, p *discovery.Project) {
	saved.ID, saved.Name, saved.Root, saved.GitCommonDir = p.ID, p.Name, p.Root, p.GitCommonDir
	known := map[string]state.Worktree{}
	for _, w := range saved.Worktrees {
		known[w.ID] = w
	}
	if saved.Instances == nil {
		saved.Instances = map[string]state.Instance{}
	}
	for _, w := range p.Worktrees {
		old := known[w.ID]
		known[w.ID] = state.Worktree{Alias: old.Alias, ID: w.ID, Root: w.Root, Branch: w.Branch, Detached: w.Detached}
		if w.Config == nil {
			continue
		}
		for name := range w.Config.Services {
			id := p.ID + "/" + w.ID + "/" + name
			if _, ok := saved.Instances[id]; !ok {
				saved.Instances[id] = state.Instance{ID: id, Service: name, WorktreeID: w.ID, State: "stopped"}
			}
		}
	}
	saved.Worktrees = nil
	for _, id := range sortedKeys(known) {
		saved.Worktrees = append(saved.Worktrees, known[id])
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
		e.fail("NOT_FOUND", "registered project not found: "+selector, "use rundevrun projects --json")
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

func listConfiguration(ctx context.Context, store *state.Store, opts options, e *envelope) {
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
			e.fail("CONFIG_INVALID", "missing rundevrun.toml in "+w.Root)
			continue
		}
		if e.Command == "config check" {
			if _, err := discovery.Environment(w.Root, w.Config.Project); err != nil {
				e.fail("CONFIG_INVALID", err.Error())
				continue
			}
		}
		for _, name := range sortedKeys(w.Config.Services) {
			id := p.ID + "/" + w.ID + "/" + name
			service := w.Config.Services[name]
			info := instanceInfo{ID: id, ProjectID: p.ID, WorktreeID: w.ID, Service: name, State: "stopped", Protocol: service.Protocol}
			if e.Command == "config check" {
				info.ConfigFile, info.WorktreeRoot = w.Config.Filename, w.Root
				info.ConfigMode = "shared"
				if w.Config.Filename != filepath.Join(w.Root, "rundevrun.toml") {
					info.ConfigMode = "external"
				}
			}
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
				applyRouteView(store, instance, &info)
			}
			addDependencyDetails(store, viewProject, state.Worktree{ID: w.ID, Root: w.Root}, &service, instance, registered, &info, e)
			if e.text {
				info.projectName, info.worktreeLabel = p.Name, worktreeName(w.Branch, w.Root)
				if e.Command == "services" {
					info.setDisplayURLs(ctx, store, instance)
				}
			}
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

func listRegistry(ctx context.Context, store *state.Store, opts options, e *envelope) {
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
	selectedWorktrees := map[string]string{}
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
			selectedWorktrees[p.ID] = w.ID
		}
		for _, key := range sortedKeys(p.Instances) {
			i := p.Instances[key]
			if selectedWorktree != "" && i.WorktreeID != selectedWorktree {
				continue
			}
			observed, unknown := inspectState(store, i, e)
			info := instanceInfo{ID: i.ID, ProjectID: p.ID, WorktreeID: i.WorktreeID, Service: i.Service, State: observed, StateUnknown: unknown, RunID: i.RunID}
			info.projectName, info.worktreeLabel = p.Name, i.WorktreeID
			info.Preparation = preparationView(store, i)
			info.Hook = hookView(store, i.Hook)
			addRunDetails(store, i, &info)
			i.State = observed
			if worktree, ok := findWorktree(p, i.WorktreeID); ok {
				info.worktreeLabel = worktreeName(worktree.Branch, worktree.Root)
				cfg, configErr := discovery.LoadConfig(worktree.Root)
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
						info.ConfigError = "service is absent from current rundevrun.toml"
						info.ConfigChanged, info.RestartRequired = configDrift(store, i, cfg.Digest, e)
						addDependencyDetails(store, p, worktree, nil, i, true, &info, e)
					}
				}
			} else {
				info.ConfigError = "registered worktree metadata is missing"
				addDependencyDetails(store, p, state.Worktree{ID: i.WorktreeID}, nil, i, true, &info, e)
			}
			applyRouteView(store, i, &info)
			if e.text {
				info.setDisplayURLs(ctx, store, i)
			}
			e.Results = append(e.Results, info)
		}
	}
	if len(e.Errors) > 0 && len(e.Results) > 0 {
		e.Status = "partial"
	}
	if e.Command == "status" {
		addOperationViews(store, ids, e)
		addPublicationViews(ctx, store, ids, selectedWorktrees, e)
	}
}

func addPublicationViews(ctx context.Context, store *state.Store, projectIDs []string, worktrees map[string]string, e *envelope) {
	items, err := (&publication.Manager{Store: store}).List(ctx)
	if err != nil {
		before := len(e.Errors)
		failError(e, err)
		if len(e.Errors) > before {
			e.PublicationErrors = append(e.PublicationErrors, e.Errors[len(e.Errors)-1])
		}
		if len(e.Results) > 0 {
			e.Status = "partial"
		}
		return
	}
	wantedProjects := make(map[string]bool, len(projectIDs))
	for _, id := range projectIDs {
		wantedProjects[id] = true
	}
	for _, item := range items {
		parts := strings.Split(item.Root.InstanceID, "/")
		if len(parts) != 3 {
			continue
		}
		if len(projectIDs) > 0 && !wantedProjects[parts[0]] {
			continue
		}
		if selected := worktrees[parts[0]]; selected != "" && parts[1] != selected {
			continue
		}
		e.Publications = append(e.Publications, item)
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
	}
	info.DirectURL, info.URL = manifest.Addresses["direct_url"], manifest.Addresses["url"]
	base := filepath.Join(store.Root, "runs", instance.RunID)
	info.LogPaths = map[string]string{"build": filepath.Join(base, "build.log"), "application": filepath.Join(base, "application.log"), "supervisor": filepath.Join(base, "supervisor.log")}
	if info.State == "starting" || info.State == "running" || info.State == "ready" || info.State == "unhealthy" {
		if record, err := store.ReadRun(instance.RunID); err == nil {
			info.PID = record.Child.PID
		}
	}
}

func localEntryURL(instance state.Instance) string {
	for _, ep := range instance.Endpoints {
		if ep.Scope == "local" && !ep.Alias && ep.InstanceID == instance.ID && ep.Port > 0 {
			return "http://" + net.JoinHostPort(ep.Hostname, strconv.Itoa(ep.Port))
		}
	}
	return instance.RouteURL
}

func applyRouteView(store *state.Store, instance state.Instance, info *instanceInfo) {
	if instance.RunID == "" || info.StateUnknown {
		if instance.RouteURL != "" {
			info.URL = instance.RouteURL
		}
		return
	}
	manifest, err := store.ReadManifest(instance.RunID)
	if err != nil || manifest.Launch.Protocol != "http" {
		if instance.RouteURL != "" {
			info.URL = instance.RouteURL
		}
		return
	}
	desiredURL := localEntryURL(instance)
	if desiredURL == "" {
		desiredURL = manifest.Addresses["direct_url"]
	}
	info.URL = desiredURL
	if liveState(info.State) && manifest.Addresses["url"] != desiredURL {
		info.RouteChanged = true
		info.RestartRequired = true
	}
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
				} else {
					addresses := copyBindings(targetManifest.Addresses)
					if targetInstance.RouteURL != "" {
						addresses["url"] = targetInstance.RouteURL
					} else {
						addresses["url"] = addresses["direct_url"]
					}
					if dependencyAddressesChanged(dependency, manifest.StopContract.Env, addresses) {
						info.RestartRequired = true
					}
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
	cfg, err := discovery.LoadConfig(worktree.Root)
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
	var routeErr *routing.Error
	var proxyErr *proxy.Error
	var diagnosticErr *diagnostics.Error
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = "CANCELLED"
	case errors.Is(err, state.ErrCorrupt):
		code = "STATE_CORRUPT"
	case errors.Is(err, state.ErrBusy):
		code = "BUSY"
	case errors.Is(err, state.ErrOwnershipUnknown):
		code = "OWNERSHIP_UNKNOWN"
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
	case errors.As(err, &routeErr):
		code = routeErr.Code
	case errors.As(err, &proxyErr):
		code = proxyErr.Code
	case errors.As(err, &diagnosticErr):
		code = diagnosticErr.Code
	}
	e.fail(code, err.Error())
	if errors.As(err, &configErr) {
		e.Errors[len(e.Errors)-1].Field = configErr.Field
	}
}
