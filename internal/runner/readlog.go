// Reads and tails service logs.
package runner

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
	"golang.org/x/sys/unix"
)

func (r *Runner) Logs(ctx context.Context, p *discovery.Project, w *discovery.Worktree, service string, tail int, follow bool, out io.Writer) error {
	if r == nil || r.Store == nil || p == nil || w == nil || out == nil || tail < 0 || tail > 10000 {
		return &Error{"USAGE", errors.New("invalid log request")}
	}
	reg, err := r.Store.Read()
	if err != nil {
		return err
	}
	project, err := reg.Project(p.ID)
	if err != nil {
		return err
	}
	instance, ok := project.Instances[p.ID+"/"+w.ID+"/"+service]
	if !ok || instance.RunID == "" {
		return os.ErrNotExist
	}
	path := filepath.Join(r.Store.Root, "runs", instance.RunID, "application.log")
	lines := make([]string, 0, tail)
	next := 0
	appendLine := func(line string) {
		if len(lines) < tail {
			lines = append(lines, line)
		} else {
			lines[next] = line
			next = (next + 1) % len(lines)
		}
	}
	var currentFile *os.File
	for _, suffix := range []string{".2", ".1", ""} {
		f, err := openReadLog(path + suffix)
		if errors.Is(err, os.ErrNotExist) && suffix != "" {
			continue
		}
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 4096), logSegmentSize+1)
		for scanner.Scan() {
			if tail > 0 {
				appendLine(scanner.Text() + "\n")
			}
		}
		scanErr := scanner.Err()
		if scanErr != nil {
			f.Close()
			return scanErr
		}
		if suffix == "" && follow {
			currentFile = f
		} else if err = f.Close(); err != nil {
			return err
		}
	}
	if tail > 0 {
		if observed, err := r.Store.Inspect(instance); err == nil && observed == "stale" {
			appendLine("[rundevrun] " + state.SupervisorLostReason + "\n")
		}
	}
	for i := range lines {
		line := lines[(next+i)%len(lines)]
		if _, err = io.WriteString(out, line); err != nil {
			if currentFile != nil {
				currentFile.Close()
			}
			return err
		}
	}
	if !follow {
		return nil
	}
	f := currentFile
	defer func() { f.Close() }()
	for {
		if _, err = io.Copy(out, f); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		current, err := os.Stat(path)
		if err != nil {
			return err
		}
		opened, err := f.Stat()
		if err != nil {
			return err
		}
		if !os.SameFile(current, opened) {
			next, err := openReadLog(path)
			if err != nil {
				return err
			}
			if err = f.Close(); err != nil {
				next.Close()
				return err
			}
			f = next
		}
	}
}
func openReadLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		f.Close()
		return nil, errors.New("unsafe log file")
	}
	return f, nil
}
