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

// build executes in a separate group and retains identities of group members.
func build(ctx context.Context, command, cwd string, env []string, log *rotatingLog, timeout time.Duration) error {
	if command == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	sink := newLogSink(log)
	cmd.Stdout = sink
	cmd.Stderr = sink
	if err := cmd.Start(); err != nil {
		return err
	}
	anchor, err := procid.Observe(os.Getpid())
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return err
	}
	leader, err := procid.Observe(cmd.Process.Pid)
	if err != nil {
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
		found, err := groupMembers(leader, []procid.Ref{anchor})
		members = mergeMembers(members, found)
		if remaining := procid.Terminate(members, time.Second); len(remaining) > 0 {
			err = errors.Join(err, fmt.Errorf("build cleanup unresolved: %v", remaining))
		}
		if err != nil {
			return err
		}
		found, err = groupMembers(leader, []procid.Ref{anchor})
		if err != nil {
			return err
		}
		if len(found) > 0 {
			return errors.New("build group still has processes")
		}
		return nil
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
			found, err := groupMembers(leader, []procid.Ref{anchor})
			if err != nil {
				cleanupErr := cleanup()
				select {
				case <-done:
				case <-time.After(time.Second):
					cleanupErr = errors.Join(cleanupErr, errors.New("build wait did not complete"))
				}
				return errors.Join(err, cleanupErr)
			}
			members = mergeMembers(members, found)
		}
	}
}
