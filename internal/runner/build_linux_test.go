//go:build linux

// Tests runner build behavior on Linux.
package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
)

func TestBuildTimeoutCleansChild(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rdr")
	if output, err := exec.Command("go", "build", "-o", binary, "../../cmd/rdr").CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v %s", err, output)
	}
	l, err := openLog(filepath.Join(dir, "build.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	err = runBuildCommand(context.Background(), "sleep 60 & echo $! > child; wait", dir, os.Environ(), l, 200*time.Millisecond, binary,
		func(procid.Ref) error { return nil }, func() error { return nil }, func([]procid.Ref) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "child"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = procid.Observe(pid); !errors.Is(err, procid.ErrGone) {
		t.Fatalf("child %d remains: %v", pid, err)
	}
}

func TestBuildExitBeforeOutputDrainPreservesExitResult(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rdr")
	if output, err := exec.Command("go", "build", "-o", binary, "../../cmd/rdr").CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v %s", err, output)
	}
	for _, code := range []int{0, 7} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			log, err := openLog(filepath.Join(t.TempDir(), "build.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			log.mu.Lock()
			locked := true
			defer func() {
				if locked {
					log.mu.Unlock()
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			persisted := make(chan procid.Ref, 1)
			done := make(chan error, 1)
			go func() {
				done <- runBuildCommand(ctx, "printf output; exit "+strconv.Itoa(code), dir, os.Environ(), log, 4*time.Second, binary,
					func(ref procid.Ref) error { persisted <- ref; return nil }, func() error { return nil }, func([]procid.Ref) error { return nil })
			}()
			var leader procid.Ref
			select {
			case leader = <-persisted:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			for {
				live, err := procid.Check(leader)
				if err != nil {
					t.Fatal(err)
				}
				if !live {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(5 * time.Millisecond):
				}
			}

			time.Sleep(100 * time.Millisecond)
			log.mu.Unlock()
			locked = false
			select {
			case err = <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if code == 0 {
				if err != nil {
					t.Fatalf("successful build: %v", err)
				}
			} else {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != code {
					t.Fatalf("lost exit %d: %v", code, err)
				}
			}
		})
	}
}
