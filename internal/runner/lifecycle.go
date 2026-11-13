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
	return r.start(ctx, p, w, service, false)
}
func (r *Runner) Restart(ctx context.Context, p *discovery.Project, w *discovery.Worktree, service string) (Result, error) {
	return r.start(ctx, p, w, service, true)
}
func (r *Runner) Stop(ctx context.Context, p *discovery.Project, w *discovery.Worktree, service string) (Result, error) {
	plan, err := r.Plan(ctx, "down", p, w, service)
	if err != nil {
		return Result{}, err
	}
	lock, err := r.Store.LockInstances(plan.InstanceID)
	if err != nil {
		return Result{}, err
	}
	defer lock.Close()
	if err = r.recoverPreparation(ctx, p.ID, plan.InstanceID); err != nil {
		return Result{}, err
	}
	plan, err = r.Plan(ctx, "down", p, w, service)
	if err != nil {
		return Result{}, err
	}
	if plan.RunID == "" {
		result := Result{InstanceID: plan.InstanceID, State: "stopped", Status: "unchanged"}
		if err := r.pruneRuns(plan.InstanceID); err != nil {
			return result, &Error{"IO_ERROR", err}
		}
		return result, nil
	}
	if err = r.stopLocked(ctx, p.ID, plan.InstanceID, plan.RunID); err != nil {
		return Result{}, err
	}
	result := Result{InstanceID: plan.InstanceID, State: "stopped", RunID: plan.RunID, Status: "completed"}
	if err := r.pruneRuns(plan.InstanceID); err != nil {
		return result, &Error{"IO_ERROR", err}
	}
	return result, nil
}

func (r *Runner) start(ctx context.Context, p *discovery.Project, w *discovery.Worktree, service string, restart bool) (result Result, retErr error) {
	action := "up"
	if restart {
		action = "restart"
	}
	plan, err := r.Plan(ctx, action, p, w, service)
	if err != nil {
		return Result{}, err
	}
	lock, err := r.Store.LockInstances(plan.InstanceID)
	if err != nil {
		return Result{}, err
	}
	defer lock.Close()
	if err = r.recoverPreparation(ctx, p.ID, plan.InstanceID); err != nil {
		return Result{}, err
	}
	plan, err = r.Plan(ctx, action, p, w, service)
	if err != nil {
		return Result{}, err
	}
	if !restart && (plan.State == "running" || plan.State == "ready" || plan.State == "unhealthy") {
		result, err := r.result(plan.InstanceID, plan.RunID, "unchanged")
		if err != nil {
			return result, err
		}
		if err = r.pruneRuns(plan.InstanceID); err != nil {
			return result, &Error{"IO_ERROR", err}
		}
		return result, nil
	}
	fresh, err := discovery.Discover(w.Root)
	if err != nil {
		return Result{}, err
	}
	current, err := discovery.Resolve(fresh, w.ID)
	if err != nil {
		return Result{}, err
	}
	if fresh.ID != p.ID {
		return Result{}, errors.New("project identity changed")
	}
	if current.Config.Digest != plan.Digest {
		return Result{}, &Error{"STATE_CONFLICT", errors.New("configuration changed during planning")}
	}
	svc := current.Config.Services[service]
	runID, err := randomID()
	if err != nil {
		return Result{}, err
	}
	operationID, err := randomID()
	if err != nil {
		return Result{}, err
	}
	port, err := r.reserve(p.ID, plan.InstanceID, runID, operationID, svc.Port)
	if err != nil {
		return Result{}, err
	}
	prepared := preparedRun{projectID: p.ID, plan: plan, runID: runID, reserved: true}
	defer func() {
		if retErr != nil && prepared.active {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cleanupErr := r.stopLocked(cleanupCtx, p.ID, plan.InstanceID, runID)
			retErr = errors.Join(retErr, cleanupErr)
			if cleanupErr == nil {
				prepared.active = false
			}
		}
		if prepared.reserved && !prepared.active {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cleanupErr := r.recoverPreparation(cleanupCtx, p.ID, plan.InstanceID)
			retErr = errors.Join(retErr, cleanupErr)
			if cleanupErr == nil {
				if err := r.pruneRuns(plan.InstanceID); err != nil {
					retErr = errors.Join(retErr, &Error{"IO_ERROR", err})
				}
			}
		}
	}()
	built, err := r.prepareLocked(ctx, p.ID, fresh, current, service, svc, plan, runID, operationID, port)
	if err != nil {
		return Result{}, err
	}
	prepared = built
	result, err = r.activateLocked(ctx, &prepared)
	if err != nil {
		return result, err
	}
	if err = r.pruneRuns(plan.InstanceID); err != nil {
		return result, &Error{"IO_ERROR", err}
	}
	return result, nil
}

type preparedRun struct {
	projectID string
	plan      Plan
	runID     string
	manifest  state.Manifest
	timeout   time.Duration
	reserved  bool
	active    bool
}

// prepareLocked requires the instance lock and an existing preparation reservation.
func (r *Runner) prepareLocked(ctx context.Context, projectID string, fresh *discovery.Project, current *discovery.Worktree, service string, svc config.Service, plan Plan, runID, operationID string, port int) (preparedRun, error) {
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
		addresses["url"] = values["url"]
		addresses["direct_url"] = values["url"]
	}
	env, err := environment(svc, values)
	if err != nil {
		return preparedRun{}, err
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
	manifest := state.Manifest{RunID: runID, InstanceID: plan.InstanceID, OperationID: operationID, Digest: current.Config.Digest, Addresses: addresses, StopContract: state.StopContract{Cwd: cwd, Command: svc.Command, RunDir: runDir, Env: env}, Launch: state.LaunchContract{Command: command, Cwd: cwd, Env: launchEnv, Protocol: svc.Protocol, Port: port, StopTimeout: svc.StopTimeout}}
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
		return preparedRun{}, &Error{"BUILD_FAILED", err}
	}

	startTimeout, _ := time.ParseDuration(svc.StartTimeout)
	if v, ok := svc.Health["timeout"].(string); ok {
		startTimeout, _ = time.ParseDuration(v)
	}
	return preparedRun{projectID: projectID, plan: plan, runID: runID, manifest: manifest, timeout: startTimeout, reserved: true}, nil
}

// activateLocked requires the same instance lock held during preparation.
func (r *Runner) activateLocked(ctx context.Context, prepared *preparedRun) (Result, error) {
	plan := prepared.plan
	runID := prepared.runID
	operationID := prepared.manifest.OperationID
	port := prepared.manifest.Launch.Port
	runDir := prepared.manifest.StopContract.RunDir
	projectID := prepared.projectID
	var err error
	if plan.RunID != "" && plan.State != "stopped" {
		if err = r.stopLocked(ctx, projectID, plan.InstanceID, plan.RunID); err != nil {
			return Result{}, err
		}
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
	supervisor.Process.Release()
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
			return Result{}, errors.New("supervisor failed")
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

func (r *Runner) reserve(projectID, id, runID, operationID string, requested any) (int, error) {
	var assigned int
	err := r.Store.Update(func(reg *state.Registry) error {
		used := map[int]string{}
		for key := range reg.Projects {
			p, err := reg.Project(key)
			if err != nil {
				return err
			}
			for key, i := range p.Instances {
				for _, port := range i.Reservations {
					if previous, ok := used[port]; ok && previous != key {
						return errors.New("conflicting reservations")
					}
					used[port] = key
				}
			}
		}
		if requested == nil {
			project, err := reg.Project(projectID)
			if err != nil {
				return err
			}
			i, ok := project.Instances[id]
			if !ok {
				return os.ErrNotExist
			}
			if i.Preparation != nil {
				return state.ErrBusy
			}
			i.Preparation = &state.Intent{RunID: runID, OperationID: operationID, Phase: "preparing"}
			project.Instances[id] = i
			return reg.SetProject(projectID, project)
		}
		start, end := 20000, 65535
		if requested == "auto" {
			project, err := reg.Project(projectID)
			if err != nil {
				return err
			}
			instance := project.Instances[id]
			if previous := instance.Reservations[instance.RunID]; previous != 0 {
				start, end = previous, previous
			}
		}
		if p, ok := requested.(int64); ok {
			start, end = int(p), int(p)
		}
		for p := start; p <= end; p++ {
			if owner, ok := used[p]; ok {
				if owner == id && start == end {
					assigned = p
					break
				}
				continue
			}
			l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
			if err != nil {
				continue
			}
			if err = l.Close(); err != nil {
				return err
			}
			assigned = p
			break
		}
		if assigned == 0 {
			return &Error{"PORT_CONFLICT", errors.New("no available port")}
		}
		project, err := reg.Project(projectID)
		if err != nil {
			return err
		}
		i, ok := project.Instances[id]
		if !ok {
			return os.ErrNotExist
		}
		if i.Reservations == nil {
			i.Reservations = map[string]int{}
		}
		if i.Preparation != nil {
			return state.ErrBusy
		}
		i.Reservations[runID] = assigned
		i.Preparation = &state.Intent{RunID: runID, OperationID: operationID, Phase: "preparing"}
		project.Instances[id] = i
		return reg.SetProject(projectID, project)
	})
	return assigned, err
}
func (r *Runner) stopLocked(ctx context.Context, projectID, id, runID string) error {
	var gate *state.Locks
	for {
		var err error
		gate, err = r.Store.LockSpawnGates(runID)
		if err == nil {
			break
		}
		if !errors.Is(err, state.ErrBusy) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(state.ErrBusy, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer gate.Close()
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
	record, err := r.Store.ReadRun(runID)
	if errors.Is(err, os.ErrNotExist) {
		if i.Intent == nil || i.Intent.RunID != runID {
			return err
		}
	} else if err != nil {
		return err
	} else {
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
	return Result{InstanceID: id, RunID: runID, State: record.State, Status: status, URLs: m.Addresses, LogPaths: map[string]string{"build": filepath.Join(r.Store.Root, "runs", runID, "build.log"), "application": filepath.Join(r.Store.Root, "runs", runID, "application.log"), "supervisor": filepath.Join(r.Store.Root, "runs", runID, "supervisor.log")}}, nil
}
