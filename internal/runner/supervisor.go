// Supervises detached service processes and writes their run records.
package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func Supervise(root, runID string) (result error) {
	ctx, cancel := signalContext()
	defer cancel()
	store := &state.Store{Root: root}
	gate, err := store.LockSpawnGates(runID)
	if err != nil {
		return err
	}
	defer gate.Close()
	manifest, err := store.ReadManifest(runID)
	if err != nil {
		return err
	}
	parts := strings.Split(manifest.InstanceID, "/")
	if len(parts) != 3 {
		return errors.New("invalid instance id")
	}
	reg, err := store.Read()
	if err != nil {
		return err
	}
	project, err := reg.Project(parts[0])
	if err != nil {
		return err
	}
	instance, ok := project.Instances[manifest.InstanceID]
	if !ok || instance.Intent == nil || instance.Intent.RunID != runID || instance.Intent.OperationID != manifest.OperationID || instance.Intent.Phase != "spawning" {
		return errors.New("spawn authorization revoked")
	}
	self, err := procid.Observe(os.Getpid())
	if err != nil {
		return err
	}
	record := state.RunRecord{Claim: "accepted", Supervisor: self, State: "starting"}
	write := func() error {
		lock, err := store.LockRunWriter(runID)
		if err != nil {
			return err
		}
		defer lock.Close()
		var releaseErr error
		if record.State == "failed" && record.Child.PID != 0 {
			if members, err := runtimeMembers(record.Child, self); err == nil && len(members) == 0 {
				releaseErr = releaseRunPort(store, parts[0], manifest.InstanceID, runID)
			}
		}
		return errors.Join(store.WriteRun(runID, record), releaseErr)
	}
	if err = write(); err != nil {
		return err
	}

	defer func() {
		if ctx.Err() != nil && record.State != "failed" && record.State != "stopped" {
			record.State = "stopped"
			result = errors.Join(result, write())
		} else if result != nil && record.State != "failed" && record.State != "stopped" {
			record.State = "failed"
			record.LastExit = &state.Exit{Reason: "supervisor failed", Time: time.Now().UTC()}
			result = errors.Join(result, write())
		}
	}()
	log, err := openLog(filepath.Join(root, "runs", runID, "application.log"))
	if err != nil {
		return err
	}
	defer func() {
		if err := log.Close(); err != nil {
			record.State = "failed"
			result = errors.Join(result, err, write())
		}
	}()
	sink := newLogSink(log)
	policy := manifest.Launch.Restart
	stopTimeout, err := time.ParseDuration(manifest.Launch.StopTimeout)
	if err != nil {
		return err
	}
	if policy != nil && (policy.Delay <= 0 || policy.MaxDelay < policy.Delay || policy.MaxAttempts <= 0 || policy.StableAfter <= 0 || policy.ReadyTimeout <= 0) {
		return errors.New("invalid restart policy in launch contract")
	}
	var attempts int
	var delay time.Duration
	if policy != nil {
		delay = policy.Delay
	}
	recordExit := func(exit state.Exit, message string) error {
		exit.Time = exit.Time.UTC()
		record.LastExit = &exit
		_, err := fmt.Fprintf(log, "[rundevrun] %s %s%s\n", exit.Time.Format(time.RFC3339), exit, message)
		return errors.Join(write(), err)
	}
	var retryLock *state.Locks
	for {
		record.State = "starting"
		done, err := spawnService(manifest.Launch, &record, write, sink)
		gate.Close()
		if retryLock != nil {
			retryLock.Close()
		}
		if err != nil {
			record.State = "failed"
			record.ErrorMessage = "supervisor startup failed"
			var pathErr *os.PathError
			if _, cwdErr := os.Stat(manifest.Launch.Cwd); cwdErr != nil {
				record.ErrorMessage = "startup cwd: " + cwdErr.Error()
			} else if errors.As(err, &pathErr) {
				record.ErrorMessage = pathErr.Error()
			}
			return errors.Join(err, recordExit(state.Exit{Reason: "start failed", Time: time.Now().UTC()}, ""))
		}
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		consumerResults := make(chan map[string]string, 1)
		consumersStarted, reachedReady := false, false
		onReady := func() {
			reachedReady = true
			if record.Restarts > 0 && policy != nil && policy.Consumers {
				consumersStarted = true
				go func() {
					defer close(consumerResults)
					consumerResults <- restartConsumers(attemptCtx, store, manifest, log)
				}()
			}
		}
		started := time.Now()
		exit, exitErr := superviseLoop(attemptCtx, manifest, &record, write, done, sink.errors, onReady, consumerResults)
		cancelAttempt()
		if consumersStarted {
			for failures := range consumerResults {
				record.ConsumerErrors = failures
			}
		}
		if ctx.Err() != nil {
			record.State = "stopped"
		}
		members, scanErr := runtimeMembers(record.Child, self)
		record.Members = mergeMembers(record.Members, members)
		if remaining := procid.Terminate(record.Members, stopTimeout); len(remaining) > 0 {
			scanErr = errors.Join(scanErr, errors.New("unresolved child processes"))
		} else {
			<-done
		}
		if ctx.Err() != nil && scanErr == nil {
			record.State = "stopped"
			return errors.Join(write(), exitErr)
		}
		record.State = "failed"
		if scanErr != nil {
			record.ErrorMessage = "service cleanup could not be verified"
		}
		if policy == nil || scanErr != nil || ctx.Err() != nil || !shouldRestart(*policy, exit) {
			return errors.Join(recordExit(exit, ""), exitErr, scanErr)
		}
		if reachedReady && exit.Reason == "process exited" && exit.Time.Sub(started) >= policy.StableAfter {
			attempts, delay = 0, policy.Delay
		}
		if attempts >= policy.MaxAttempts {
			return recordExit(exit, "; retry limit reached")
		}
		attempts++
		record.State = "restarting"
		if err := recordExit(exit, fmt.Sprintf("; restarting in %s (attempt %d)", delay, attempts)); err != nil {
			record.State = "failed"
			return errors.Join(err, write())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		delay += min(delay, policy.MaxDelay-delay)
		gate, retryLock, err = lockRetry(ctx, store, manifest, parts[0])
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			record.State = "failed"
			return errors.Join(err, write())
		}
		record.Restarts++
	}
}

func lockRetry(ctx context.Context, store *state.Store, manifest state.Manifest, projectID string) (gate, instanceLock *state.Locks, err error) {
	for ctx.Err() == nil {
		reg, err := store.Read()
		if err != nil {
			return nil, nil, err
		}
		project, err := reg.Project(projectID)
		if err != nil {
			return nil, nil, err
		}
		instance := project.Instances[manifest.InstanceID]
		if instance.RunID != manifest.RunID || instance.Intent == nil || instance.Intent.OperationID != manifest.OperationID ||
			(instance.Intent.Phase != "started" && instance.Intent.Phase != "spawning") {
			return nil, nil, errors.New("restart authorization revoked")
		}
		if instance.Intent.Phase == "started" {
			instanceLock, err = store.LockInstances(manifest.InstanceID)
		}
		if err == nil {
			gate, err = store.LockSpawnGates(manifest.RunID)
			if err == nil && ctx.Err() == nil {
				return gate, instanceLock, nil
			}
			if gate != nil {
				gate.Close()
			}
			if instanceLock != nil {
				instanceLock.Close()
				instanceLock = nil
			}
		}
		if err != nil && !errors.Is(err, state.ErrBusy) {
			return nil, nil, err
		}
		select {
		case <-ctx.Done():
		case <-time.After(25 * time.Millisecond):
		}
	}
	return nil, nil, ctx.Err()
}

func shouldRestart(policy state.RestartPolicy, exit state.Exit) bool {
	switch exit.Reason {
	case "readiness timeout":
		return policy.Mode == "always" || policy.Mode == "on-failure"
	case "process exited":
		return policy.Mode == "always" || (policy.Mode == "on-failure" && (exit.Signal != "" || (exit.Code != nil && *exit.Code != 0)))
	default:
		return false
	}
}

type processExit struct {
	status *os.ProcessState
	time   time.Time
	err    error
}

func spawnService(launch state.LaunchContract, record *state.RunRecord, write func() error, sink *logSink) (<-chan processExit, error) {
	read, ack, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer read.Close()
	defer ack.Close()
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(binary, "__rdr_bootstrap")
	cmd.Dir = launch.Cwd
	cmd.Env = append(append([]string(nil), launch.Env...), "__RDR_COMMAND="+launch.Command)
	cmd.ExtraFiles = []*os.File{read}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = sink, sink
	cmd.WaitDelay = 100 * time.Millisecond
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	read.Close()
	done := make(chan processExit, 1)
	go func() {
		defer close(done)
		err := cmd.Wait()
		done <- processExit{cmd.ProcessState, time.Now(), err}
	}()
	abort := func(err error) (<-chan processExit, error) {
		_ = cmd.Process.Kill()
		<-done
		return nil, err
	}
	child, err := procid.Observe(cmd.Process.Pid)
	if err != nil {
		return abort(err)
	}
	record.Child, record.Members = child, []procid.Ref{child}
	if err = write(); err != nil {
		return abort(err)
	}
	if _, err = ack.Write([]byte{1}); err != nil {
		return abort(err)
	}
	return done, nil
}

func superviseLoop(ctx context.Context, m state.Manifest, record *state.RunRecord, write func() error, done <-chan processExit, logErrors <-chan error, onReady func(), consumerResults <-chan map[string]string) (exit state.Exit, result error) {
	defer func() {
		if exit.Time.IsZero() {
			exit = state.Exit{Reason: "supervisor failed", Time: time.Now().UTC()}
		}
	}()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var readyTimeout <-chan time.Time
	if m.Launch.Restart != nil {
		timer := time.NewTimer(m.Launch.Restart.ReadyTimeout)
		defer timer.Stop()
		readyTimeout = timer.C
	}
	ready := false
	unhealthy := func() error {
		if record.State == "ready" || record.State == "running" {
			record.State = "unhealthy"
			return write()
		}
		return nil
	}
	for {
		select {
		case err := <-logErrors:
			return state.Exit{Reason: "log write failed", Time: time.Now().UTC()}, err
		case failure := <-done:
			exit := state.Exit{Reason: "process exited", Time: failure.time}
			if failure.status != nil {
				status := failure.status.Sys().(syscall.WaitStatus)
				if status.Signaled() {
					exit.Signal = status.Signal().String()
				} else if status.Exited() {
					code := status.ExitStatus()
					exit.Code = &code
				}
			}
			return exit, failure.err
		case <-ctx.Done():
			return state.Exit{Reason: "stopped by rundevrun", Time: time.Now().UTC()}, nil
		case <-readyTimeout:
			return state.Exit{Reason: "readiness timeout", Time: time.Now().UTC()}, nil
		case failures := <-consumerResults:
			consumerResults = nil
			record.ConsumerErrors = failures
			if err := write(); err != nil {
				return state.Exit{}, err
			}
		case <-tick.C:
			members, err := runtimeMembers(record.Child, record.Supervisor)
			if err != nil {
				return state.Exit{}, err
			}
			before := len(record.Members)
			record.Members = mergeMembers(record.Members, members)
			if len(record.Members) != before {
				if err := write(); err != nil {
					return state.Exit{}, err
				}
			}
			if err = checkListeners(record.Members, m.Launch.Port); err != nil {

				if !strings.HasPrefix(err.Error(), "no owned loopback listener") {
					return state.Exit{}, err
				}
				if err := unhealthy(); err != nil {
					return state.Exit{}, err
				}
				continue
			}
			stateName := "running"
			if m.Launch.HealthPath != "" {
				client := http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
				response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(m.Launch.Port) + m.Launch.HealthPath)
				if err != nil {
					if err := unhealthy(); err != nil {
						return state.Exit{}, err
					}
					continue
				}
				response.Body.Close()
				if response.StatusCode < 200 || response.StatusCode >= 300 {
					if err := unhealthy(); err != nil {
						return state.Exit{}, err
					}
					continue
				}
				stateName = "ready"
			} else if m.Launch.HealthTCP {
				conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(m.Launch.Port)), time.Second)
				if err != nil {
					if err := unhealthy(); err != nil {
						return state.Exit{}, err
					}
					continue
				}
				conn.Close()
				stateName = "ready"
			}
			record.State = stateName
			if err := write(); err != nil {
				return state.Exit{}, err
			}
			if !ready {
				ready = true
				readyTimeout = nil
				onReady()
			}
		}
	}
}

func mergeMembers(old, next []procid.Ref) []procid.Ref {
	for _, r := range next {
		found := false
		for _, x := range old {
			if x == r {
				found = true
				break
			}
		}
		if !found {
			old = append(old, r)
		}
	}
	return old
}
