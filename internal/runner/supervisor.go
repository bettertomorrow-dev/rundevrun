package runner

import (
	"errors"
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

// Supervise runs in a detached rdr process. Only this process writes its run record.
func Supervise(root, runID string) (result error) {
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
		return store.WriteRun(runID, record)
	}
	if err = write(); err != nil {
		return err
	}
	// A claimed supervisor must leave a durable terminal record when startup fails.
	startup := true
	defer func() {
		if startup && result != nil {
			record.State = "failed"
			if record.ErrorMessage == "" {
				record.ErrorMessage = "supervisor startup failed"
				var pathErr *os.PathError
				if errors.As(result, &pathErr) {
					record.ErrorMessage = pathErr.Error()
				}
			}
			if e := write(); e != nil {
				result = errors.Join(result, e)
			}
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
	read, ack, err := os.Pipe()
	if err != nil {
		return err
	}
	defer ack.Close()
	binary, err := os.Executable()
	if err != nil {
		read.Close()
		return err
	}
	cmd := exec.Command(binary, "__rdr_bootstrap")
	cmd.Dir = manifest.Launch.Cwd
	cmd.Env = append(append([]string(nil), manifest.Launch.Env...), "__RDR_COMMAND="+manifest.Launch.Command)
	cmd.ExtraFiles = []*os.File{read}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	sink := newLogSink(log)
	cmd.Stdout = sink
	cmd.Stderr = sink
	if err = cmd.Start(); err != nil {
		if _, cwdErr := os.Stat(manifest.Launch.Cwd); cwdErr != nil {
			record.ErrorMessage = "startup cwd: " + cwdErr.Error()
		}
		read.Close()
		return err
	}
	read.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	child, err := procid.Observe(cmd.Process.Pid)
	if err != nil {
		cmd.Process.Kill()
		<-done
		return err
	}
	record.Child = child
	record.Members = []procid.Ref{child}
	if err = write(); err != nil {
		cmd.Process.Kill()
		<-done
		return err
	}
	if _, err = ack.Write([]byte{1}); err != nil {
		cmd.Process.Kill()
		<-done
		return err
	}
	if err = ack.Close(); err != nil {
		cmd.Process.Kill()
		<-done
		return err
	}
	if err = gate.Close(); err != nil {
		cmd.Process.Kill()
		<-done
		return err
	}
	startup = false
	return superviseLoop(manifest, &record, write, done, sink.errors)
}

func superviseLoop(m state.Manifest, record *state.RunRecord, write func() error, done <-chan error, logErrors <-chan error) (result error) {
	ctx, cancel := signalContext()
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	stopTimeout, err := time.ParseDuration(m.Launch.StopTimeout)
	if err != nil {
		return err
	}
	defer func() {
		if result == nil {
			return
		}
		members, scanErr := groupMembers(record.Child, []procid.Ref{record.Supervisor})
		record.Members = mergeMembers(record.Members, members)
		if scanErr != nil {
			result = errors.Join(result, scanErr)
		}
		if remaining := procid.Terminate(record.Members, stopTimeout); len(remaining) > 0 {
			result = errors.Join(result, errors.New("unresolved child processes"))
		}
		record.State = "failed"
		if err := write(); err != nil {
			result = errors.Join(result, err)
		}
	}()
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
			return err
		case err := <-done:
			members, scanErr := groupMembers(record.Child, []procid.Ref{record.Supervisor})
			if scanErr != nil {
				return scanErr
			}
			record.Members = mergeMembers(record.Members, members)
			alive := procid.Terminate(record.Members, stopTimeout)
			record.State = "failed"
			if ctx.Err() != nil && len(alive) == 0 {
				record.State = "stopped"
			}
			if e := write(); e != nil {
				return e
			}
			if len(alive) > 0 {
				return errors.New("unresolved child processes")
			}
			return err
		case <-ctx.Done():
			members, err := groupMembers(record.Child, []procid.Ref{record.Supervisor})
			if err != nil {
				return err
			}
			record.Members = mergeMembers(record.Members, members)
			alive := procid.Terminate(record.Members, stopTimeout)
			if len(alive) > 0 {
				return errors.New("unresolved child processes")
			}
			record.State = "stopped"
			if err := write(); err != nil {
				return err
			}
			return nil
		case <-tick.C:
			members, err := groupMembers(record.Child, []procid.Ref{record.Supervisor})
			if err != nil {
				return err
			}
			before := len(record.Members)
			record.Members = mergeMembers(record.Members, members)
			if len(record.Members) != before {
				if err := write(); err != nil {
					return err
				}
			}
			if err = checkListeners(record.Members, m.Launch.Port); err != nil {
				// During startup a listener may not yet exist. Non-loopback sockets fail closed.
				if !strings.HasPrefix(err.Error(), "no owned loopback listener") {
					return err
				}
				if err := unhealthy(); err != nil {
					return err
				}
				continue
			}
			stateName := "running"
			if m.Launch.HealthPath != "" {
				client := http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
				response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(m.Launch.Port) + m.Launch.HealthPath)
				if err != nil {
					if err := unhealthy(); err != nil {
						return err
					}
					continue
				}
				response.Body.Close()
				if response.StatusCode < 200 || response.StatusCode >= 300 {
					if err := unhealthy(); err != nil {
						return err
					}
					continue
				}
				stateName = "ready"
			} else if m.Launch.HealthTCP {
				conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(m.Launch.Port)), time.Second)
				if err != nil {
					if err := unhealthy(); err != nil {
						return err
					}
					continue
				}
				conn.Close()
				stateName = "ready"
			}
			record.State = stateName
			if err := write(); err != nil {
				return err
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
