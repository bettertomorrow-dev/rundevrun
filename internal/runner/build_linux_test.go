//go:build linux

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
