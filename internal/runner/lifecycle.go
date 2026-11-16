// Prepares, starts, and stops supervised service runs.
package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func (r *Runner) Up(ctx context.Context, p *discovery.Project, w *discovery.Worktree, service string) (Result, error) {
	g, err := r.UpGraph(ctx, []Selection{{p, w, service}})
	id, _ := selectionID(Selection{p, w, service})
	for _, result := range g.Results {
		if result.InstanceID == id {
			return result, err
		}
	}
	return Result{}, err
}
func (r *Runner) Restart(ctx context.Context, p *discovery.Project, w *discovery.Worktree, service string) (Result, error) {
	g, err := r.RestartGraph(ctx, []Selection{{p, w, service}})
	id, _ := selectionID(Selection{p, w, service})
	for _, result := range g.Results {
		if result.InstanceID == id {
			return result, err
		}
	}
	return Result{}, err
}
func (r *Runner) Stop(ctx context.Context, p *discovery.Project, w *discovery.Worktree, service string) (Result, error) {
	g, err := r.DownGraph(ctx, []Selection{{p, w, service}}, false)
	id, _ := selectionID(Selection{p, w, service})
	for _, result := range g.Results {
		if result.InstanceID == id {
			return result, err
		}
	}
	return Result{}, err
}

type preparedRun struct {
	projectID   string
	plan        Plan
	runID       string
	manifest    state.Manifest
	timeout     time.Duration
	reason      string
	oldWorktree string
	newWorktree string
	reserved    bool
	active      bool
}

func (r *Runner) prepareLocked(ctx context.Context, projectID string, fresh *discovery.Project, current *discovery.Worktree, service string, svc config.Service, plan Plan, runID, operationID string, port int, dependencies map[string]map[string]string, bindings map[string]string) (preparedRun, error) {
	runDir := filepath.Join(r.Store.Root, "runs", runID)
	values := map[string]string{"host": "127.0.0.1", "run_dir": runDir, "worktree": current.Root, "project": fresh.Name, "service": service}
	addresses := map[string]string{"host": "127.0.0.1"}
	if port != 0 {
		values["port"] = strconv.Itoa(port)
		addresses["port"] = values["port"]
	}
	if svc.Protocol == "http" {
		values["url"] = "http://127.0.0.1:" + values["port"]
		values["direct_url"] = values["url"]
		if plan.Addresses["url"] != "" {
			values["url"] = plan.Addresses["url"]
		}
		addresses["url"] = values["url"]
		addresses["direct_url"] = values["direct_url"]
	}
	for dep, addresses := range dependencies {
		for k, v := range addresses {
			if k != "tentative" {
				values["dependencies."+dep+"."+k] = v
			}
		}
	}
	env, err := environment(svc, values)
	if err != nil {
		return preparedRun{}, err
	}
	for dep, addresses := range dependencies {
		norm := strings.ToUpper(strings.ReplaceAll(dep, "-", "_"))
		for k, v := range addresses {
			if k != "tentative" {
				env["RDR_DEP_"+norm+"_"+strings.ToUpper(k)] = v
			}
		}
	}
	command, subs, err := substitute(svc.Command, values, true)
	if err != nil {
		return preparedRun{}, err
	}
	launchEnv := mergeEnv(env, subs)
	buildCommand, buildSubs, err := substitute(svc.Build, values, true)
	if err != nil {
		return preparedRun{}, err
	}
	cwd, err := filepath.EvalSymlinks(filepath.Join(current.Root, svc.CWD))
	if err != nil {
		return preparedRun{}, err
	}
	manifest := state.Manifest{RunID: runID, InstanceID: plan.InstanceID, OperationID: operationID, Digest: current.Config.Digest, Addresses: addresses, EffectiveBindings: bindings, StopContract: state.StopContract{Cwd: cwd, Command: svc.Command, RunDir: runDir, Env: env}, Launch: state.LaunchContract{Command: command, Cwd: cwd, Env: launchEnv, Protocol: svc.Protocol, Port: port, StopTimeout: svc.StopTimeout}}
	for _, route := range svc.Routes {
		manifest.Routes = append(manifest.Routes, state.Route{Path: route.Path, Dependency: route.Dependency, StripPrefix: route.StripPrefix})
	}
	manifest.StopContract.Worktree = current.Root
	manifest.StopContract.HookTimeout = svc.HookTimeout
	manifest.StopContract.Hooks = map[string]string{}
	manifest.StopContract.HookEnv = map[string]map[string]string{}
	for name, hook := range svc.Hooks {
		command, subs, e := substitute(hook, values, true)
		if e != nil {
			return preparedRun{}, e
		}
		manifest.StopContract.Hooks[name] = command
		manifest.StopContract.HookEnv[name] = subs
	}
	if v, ok := svc.Health["http"].(string); ok {
		manifest.Launch.HealthPath = v
	}
	if v, ok := svc.Health["tcp"].(bool); ok {
		manifest.Launch.HealthTCP = v
	}
	if err = r.Store.WriteManifest(runID, manifest); err != nil {
		return preparedRun{}, err
	}
	buildLog, err := openLog(filepath.Join(runDir, "build.log"))
	if err != nil {
		return preparedRun{}, err
	}
	timeout, _ := time.ParseDuration(svc.BuildTimeout)
	var buildErr error
	var buildRef procid.Ref
	if buildCommand != "" {
		gate, err := r.Store.LockSpawnGates(runID)
		if err != nil {
			buildLog.Close()
			return preparedRun{}, err
		}
		binary := r.Executable
		if binary == "" {
			binary, err = os.Executable()
		}
		if err != nil {
			gate.Close()
			buildLog.Close()
			return preparedRun{}, err
		}
		buildErr = runBuildCommand(ctx, buildCommand, cwd, mergeEnv(env, buildSubs), buildLog, timeout, binary, func(ref procid.Ref) error {
			buildRef = ref
			return r.Store.WritePreparation(runID, state.PreparationRecord{Child: ref})
		}, gate.Close, func(members []procid.Ref) error {
			return r.Store.WritePreparation(runID, state.PreparationRecord{Child: buildRef, Members: members})
		})
		gate.Close()
		if buildErr == nil {
			buildErr = r.Store.WritePreparation(runID, state.PreparationRecord{Child: buildRef, Complete: true})
		}
	}
	closeErr := buildLog.Close()
	if err = errors.Join(buildErr, closeErr); err != nil {
		return preparedRun{}, &Error{"BUILD_FAILED", &os.PathError{Op: "build", Path: filepath.Join(runDir, "build.log"), Err: err}}
	}

	startTimeout, _ := time.ParseDuration(svc.StartTimeout)
	if v, ok := svc.Health["timeout"].(string); ok {
		startTimeout, _ = time.ParseDuration(v)
	}
	return preparedRun{projectID: projectID, plan: plan, runID: runID, manifest: manifest, timeout: startTimeout, reserved: true}, nil
}

func (r *Runner) activateLocked(ctx context.Context, prepared *preparedRun) (Result, error) {
	plan := prepared.plan
	runID := prepared.runID
	operationID := prepared.manifest.OperationID
	port := prepared.manifest.Launch.Port
	runDir := prepared.manifest.StopContract.RunDir
	projectID := prepared.projectID
	var err error
	if plan.RunID != "" && plan.State != "stopped" {
		if err = r.stopWithHooks(ctx, projectID, plan.InstanceID, plan.RunID, "restart", prepared.manifest.StopContract.Worktree, operationID); err != nil {
			return Result{}, err
		}
	}
	reason := prepared.reason
	if reason == "" {
		reason = "up"
	}
	oldWorktree := ""
	if plan.RunID != "" && plan.State != "stopped" {
		oldWorktree = prepared.manifest.StopContract.Worktree
	}
	newWorktree := prepared.manifest.StopContract.Worktree
	if reason == "switch" {
		oldWorktree, newWorktree = prepared.oldWorktree, prepared.newWorktree
	}
	if err = r.runHook(ctx, projectID, prepared.manifest, "before_start", reason, oldWorktree, newWorktree, operationID); err != nil {
		return Result{}, err
	}
	if port != 0 {
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return Result{}, &Error{"PORT_CONFLICT", err}
		}
		if err := listener.Close(); err != nil {
			return Result{}, err
		}
	}
	gate, err := r.Store.LockSpawnGates(runID)
	if err != nil {
		return Result{}, err
	}
	err = r.Store.Update(func(reg *state.Registry) error {
		project, err := reg.Project(projectID)
		if err != nil {
			return err
		}
		instance := project.Instances[plan.InstanceID]
		if instance.Preparation == nil || instance.Preparation.RunID != runID {
			return errors.New("preparation changed")
		}
		instance.Preparation = nil
		instance.RunID = runID
		instance.EffectiveBindings = prepared.manifest.EffectiveBindings
		instance.State = "starting"
		instance.Intent = &state.Intent{OperationID: operationID, RunID: runID, Phase: "spawning"}
		project.Instances[plan.InstanceID] = instance
		return reg.SetProject(projectID, project)
	})
	if err == nil {
		prepared.active = true
	}
	gateErr := gate.Close()
	if err = errors.Join(err, gateErr); err != nil {
		return Result{}, err
	}
	binary := r.Executable
	if binary == "" {
		binary, err = os.Executable()
		if err != nil {
			return Result{}, err
		}
	}
	supervisor := exec.Command(binary, "__rdr_supervisor", r.Store.Root, runID)
	supervisor.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	log, err := openLog(filepath.Join(runDir, "supervisor.log"))
	if err != nil {
		return Result{}, err
	}
	supervisor.Stdout = log.file
	supervisor.Stderr = log.file
	if err = supervisor.Start(); err != nil {
		log.Close()
		return Result{}, err
	}

	go func() { _ = supervisor.Wait() }()
	if err = log.Close(); err != nil {
		return Result{}, err
	}
	timeout := prepared.timeout
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		record, readErr := r.Store.ReadRun(runID)
		if readErr == nil && (record.State == "running" || record.State == "ready") {
			if err = r.Store.Update(func(reg *state.Registry) error {
				project, err := reg.Project(projectID)
				if err != nil {
					return err
				}
				i := project.Instances[plan.InstanceID]
				if i.RunID != runID {
					return errors.New("run changed")
				}
				i.State = record.State
				i.Intent.Phase = "started"
				project.Instances[i.ID] = i
				return reg.SetProject(projectID, project)
			}); err != nil {
				return Result{}, err
			}
			prepared.reserved = false
			prepared.active = false
			if err = r.runHook(ctx, projectID, prepared.manifest, "after_start", reason, oldWorktree, newWorktree, operationID); err != nil {
				return Result{}, err
			}
			result, err := r.result(plan.InstanceID, runID, "completed")
			if err != nil {
				return result, err
			}
			return result, nil
		}
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return Result{}, readErr
		}
		if readErr == nil && record.State == "failed" {
			message := record.ErrorMessage
			if message == "" {
				message = "supervisor failed"
			}
			return Result{}, &Error{"PROCESS_FAILED", errors.New(message)}
		}
		select {
		case <-waitCtx.Done():
			return Result{}, &Error{"READINESS_TIMEOUT", waitCtx.Err()}
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func environment(s config.Service, values map[string]string) (map[string]string, error) {
	env := map[string]string{}
	for _, entry := range os.Environ() {
		k, v, ok := strings.Cut(entry, "=")
		if ok && !strings.HasPrefix(k, "RDR_") && !strings.HasPrefix(k, "__RDR_") {
			env[k] = v
		}
	}
	for k, v := range s.Env {
		expanded, _, err := substitute(v, values, false)
		if err != nil {
			return nil, err
		}
		env[k] = expanded
	}
	env["RDR_HOST"] = values["host"]
	if v := values["port"]; v != "" {
		env["RDR_PORT"] = v
	}
	if v := values["url"]; v != "" {
		env["RDR_URL"] = v
		env["RDR_DIRECT_URL"] = values["direct_url"]
	}
	return env, nil
}
func mergeEnv(base, extra map[string]string) []string {
	out := make([]string, 0, len(base)+len(extra))
	for k, v := range base {
		out = append(out, k+"="+v)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

func (r *Runner) lockSpawnGate(ctx context.Context, runID string) (*state.Locks, error) {
	var gate *state.Locks
	for {
		var err error
		gate, err = r.Store.LockSpawnGates(runID)
		if err == nil {
			break
		}
		if !errors.Is(err, state.ErrBusy) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(state.ErrBusy, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	return gate, nil
}
func (r *Runner) stopLocked(ctx context.Context, projectID, id, runID string) error {
	gate, err := r.lockSpawnGate(ctx, runID)
	if err != nil {
		return err
	}
	defer gate.Close()
	return r.stopUnderGate(ctx, projectID, id, runID)
}

func (r *Runner) stopUnderGate(ctx context.Context, projectID, id, runID string) error {
	reg, err := r.Store.Read()
	if err != nil {
		return err
	}
	p, err := reg.Project(projectID)
	if err != nil {
		return err
	}
	i, ok := p.Instances[id]
	if !ok || i.RunID != runID {
		return errors.New("run changed during stop")
	}
	manifest, err := r.Store.ReadManifest(runID)
	if err != nil {
		return err
	}
	if manifest.InstanceID != id {
		return &Error{"STATE_CORRUPT", errors.New("manifest instance mismatch")}
	}
	record, err := r.Store.ReadRun(runID)
	if errors.Is(err, os.ErrNotExist) {
		if i.Intent == nil || i.Intent.RunID != runID || i.Intent.OperationID != manifest.OperationID || !((i.State == "starting" && i.Intent.Phase == "spawning") || (i.State == "stopped" && i.Intent.Phase == "stopped")) {
			return &Error{"OWNERSHIP_UNKNOWN", errors.New("missing supervisor record for an uncertain run")}
		}
	} else if err != nil {
		return err
	} else {
		if record.Claim == "accepted" && record.Child == (procid.Ref{}) {
			sup := record.Supervisor
			validIntent := i.Intent != nil && i.Intent.RunID == runID && i.Intent.OperationID == manifest.OperationID && ((i.State == "starting" && i.Intent.Phase == "spawning") || (i.State == "stopped" && i.Intent.Phase == "stopped"))
			if !validIntent || (record.State != "starting" && record.State != "failed" && record.State != "stopped") || len(record.Members) != 0 || sup.PID <= 1 || sup.PGID != sup.PID || sup.SID != sup.PID || sup.Identity == "" || sup.BootID == "" {
				return &Error{"OWNERSHIP_UNKNOWN", errors.New("incomplete unspawned claim")}
			}
			self, e := procid.Observe(os.Getpid())
			if e != nil {
				return &Error{"OWNERSHIP_UNKNOWN", e}
			}
			if sup.BootID != self.BootID {
				return &Error{"OWNERSHIP_UNKNOWN", errors.New("unspawned supervisor boot mismatch")}
			}
			live, e := procid.Check(sup)
			if e != nil {
				return &Error{"OWNERSHIP_UNKNOWN", e}
			}
			if live {
				return &Error{"OWNERSHIP_UNKNOWN", errors.New("unspawned supervisor is still alive")}
			}

			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			for {
				if e = verifySessionGone(sup); e == nil {
					break
				}
				select {
				case <-ctx.Done():
					return &Error{"OWNERSHIP_UNKNOWN", errors.Join(e, ctx.Err())}
				case <-deadline.C:
					return &Error{"OWNERSHIP_UNKNOWN", e}
				case <-time.After(25 * time.Millisecond):
				}
			}
			goto stopped
		}
		if record.Claim == "accepted" && record.Supervisor.PID == 0 {
			return &Error{"OWNERSHIP_UNKNOWN", errors.New("claimed run missing supervisor identity")}
		}
		if record.Supervisor.PID != 0 {
			live, checkErr := procid.Check(record.Supervisor)
			if checkErr != nil {
				return &Error{"OWNERSHIP_UNKNOWN", checkErr}
			}
			if !live && record.State != "stopped" && record.State != "failed" {
				return &Error{"OWNERSHIP_UNKNOWN", errors.New("supervisor disappeared before confirmed cleanup")}
			}
			if live {
				members, scanErr := groupMembers(record.Child, []procid.Ref{record.Supervisor})
				if record.Child.PID != 0 && scanErr != nil {
					return scanErr
				}
				record.Members = mergeMembers(record.Members, members)
				if err = syscall.Kill(record.Supervisor.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
					return err
				}
				stopTimeout, err := time.ParseDuration(manifest.Launch.StopTimeout)
				if err != nil {
					return err
				}
				deadline := time.NewTimer(stopTimeout)
				defer deadline.Stop()
				for {
					live, err = procid.Check(record.Supervisor)
					if err != nil {
						return &Error{"OWNERSHIP_UNKNOWN", err}
					}
					if !live {
						break
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-deadline.C:
						if remaining := procid.Terminate([]procid.Ref{record.Supervisor}, 0); len(remaining) > 0 {
							return &Error{"OWNERSHIP_UNKNOWN", errors.New("supervisor cleanup unresolved")}
						}
					case <-time.After(20 * time.Millisecond):
					}
				}
			}
		}
		timeout, err := time.ParseDuration(manifest.Launch.StopTimeout)
		if err != nil {
			return err
		}
		refs := append([]procid.Ref(nil), record.Members...)
		if record.Child != (procid.Ref{}) {
			refs = append(refs, record.Child)
		}
		if remaining := procid.Terminate(refs, timeout); len(remaining) > 0 {
			return &Error{"OWNERSHIP_UNKNOWN", errors.New("child cleanup unresolved")}
		}
		checked := map[int]bool{}
		if len(refs) == 0 && record.Supervisor != (procid.Ref{}) {
			refs = append(refs, record.Supervisor)
		}
		for _, ref := range refs {
			if ref == (procid.Ref{}) || checked[ref.PGID] {
				continue
			}
			checked[ref.PGID] = true
			if err := verifySessionGone(ref); err != nil {
				return err
			}
		}
	}
stopped:
	return r.Store.Update(func(reg *state.Registry) error {
		p, err := reg.Project(projectID)
		if err != nil {
			return err
		}
		i := p.Instances[id]
		if i.RunID != runID {
			return errors.New("run changed during stop")
		}
		i.State = "stopped"
		if i.Intent != nil {
			i.Intent.Phase = "stopped"
		}
		delete(i.Reservations, runID)
		p.Instances[id] = i
		return reg.SetProject(projectID, p)
	})
}
func (r *Runner) result(id, runID, status string) (Result, error) {
	m, err := r.Store.ReadManifest(runID)
	if err != nil {
		return Result{}, err
	}
	record, err := r.Store.ReadRun(runID)
	if err != nil {
		return Result{}, err
	}
	urls := make(map[string]string, len(m.Addresses))
	for k, v := range m.Addresses {
		urls[k] = v
	}
	if urls["direct_url"] != "" {
		urls["url"] = urls["direct_url"]
		if r.Routing != nil && r.Routing.Routing.Enabled {
			parts := strings.Split(id, "/")
			if len(parts) != 3 {
				return Result{}, errors.New("invalid instance id")
			}
			reg, e := r.Store.Read()
			if e != nil {
				return Result{}, e
			}
			project, e := reg.Project(parts[0])
			if e != nil {
				return Result{}, e
			}
			if u := project.Instances[id].RouteURL; u != "" {
				urls["url"] = u
			}
		}
	}
	return Result{InstanceID: id, RunID: runID, State: record.State, Status: status, URLs: urls, LogPaths: map[string]string{"build": filepath.Join(r.Store.Root, "runs", runID, "build.log"), "application": filepath.Join(r.Store.Root, "runs", runID, "application.log"), "supervisor": filepath.Join(r.Store.Root, "runs", runID, "supervisor.log")}}, nil
}
