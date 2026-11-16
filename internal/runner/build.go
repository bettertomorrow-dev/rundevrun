// Builds service artifacts and records build results.
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
)

func runBuildCommand(ctx context.Context, command, cwd string, env []string, log *rotatingLog, timeout time.Duration, binary string, persist func(procid.Ref) error, releaseGate func() error, persistMembers func([]procid.Ref) error) error {
	if command == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	read, ack, err := os.Pipe()
	if err != nil {
		return err
	}
	defer read.Close()
	defer ack.Close()
	cmd := exec.Command(binary, "__rdr_bootstrap")
	cmd.ExtraFiles = []*os.File{read}
	env = append(append([]string(nil), env...), "__RDR_COMMAND="+command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Dir = cwd
	cmd.Env = env
	sink := newLogSink(log)
	cmd.Stdout = sink
	cmd.Stderr = sink
	if err := cmd.Start(); err != nil {
		return err
	}
	leader, err := procid.Observe(cmd.Process.Pid)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return err
	}
	read.Close()
	if err := persist(leader); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return err
	}
	if _, err := ack.Write([]byte{1}); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return err
	}
	if err := ack.Close(); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return err
	}
	if err := releaseGate(); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return err
	}
	members := []procid.Ref{leader}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	cleanup := func() error {
		found, err := groupMembers(leader, members)
		if err != nil {
			if ok, e := procid.Check(leader); e != nil {
				err = e
			} else if !ok {
				err = verifySessionGone(leader)
			}
		}
		members = mergeMembers(members, found)
		if remaining := procid.Terminate(members, time.Second); len(remaining) > 0 {
			err = errors.Join(err, fmt.Errorf("build cleanup unresolved: %v", remaining))
		}
		if err != nil {
			return err
		}
		return verifySessionGone(leader)
	}
	for {
		select {
		case err := <-sink.errors:
			cleanupErr := cleanup()
			select {
			case <-done:
			case <-time.After(time.Second):
				cleanupErr = errors.Join(cleanupErr, errors.New("build wait did not complete"))
			}
			return errors.Join(err, cleanupErr)
		case err := <-done:
			cleanupErr := cleanup()
			if err != nil {
				return errors.Join(err, cleanupErr)
			}
			return cleanupErr
		case <-ctx.Done():
			cleanupErr := cleanup()
			select {
			case <-done:
			case <-time.After(time.Second):
				cleanupErr = errors.Join(cleanupErr, errors.New("build wait did not complete"))
			}
			return errors.Join(ctx.Err(), cleanupErr)
		case <-tick.C:
			found, err := groupMembers(leader, members)
			if err != nil {

				if verifySessionGone(leader) == nil {
					continue
				}
				cleanupErr := cleanup()
				select {
				case <-done:
				case <-time.After(time.Second):
					cleanupErr = errors.Join(cleanupErr, errors.New("build wait did not complete"))
				}
				return errors.Join(err, cleanupErr)
			}
			before := len(members)
			members = mergeMembers(members, found)
			if len(members) != before {
				if err := persistMembers(members); err != nil {
					cleanupErr := cleanup()
					select {
					case <-done:
					case <-time.After(time.Second):
						cleanupErr = errors.Join(cleanupErr, errors.New("build wait did not complete"))
					}
					return errors.Join(err, cleanupErr)
				}
			}
		}
	}
}
