//go:build linux

package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
)

func TestBuildTimeoutCleansChild(t *testing.T) {
	dir := t.TempDir()
	l, err := openLog(filepath.Join(dir, "build.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	err = build(context.Background(), "sleep 60 & echo $! > child; wait", dir, os.Environ(), l, 200*time.Millisecond)
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
