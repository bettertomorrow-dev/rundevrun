//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func deadSession(t *testing.T) procid.Ref {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ref, err := procid.Observe(cmd.Process.Pid)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatal(err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	return ref
}
func TestRetentionProtectsReferencedLiveUnknownAndForeign(t *testing.T) {
	dir := t.TempDir()
	store, err := state.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := "p/w/s"
	other := "p/w/t"
	dead := deadSession(t)
	err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject("p", state.Project{ID: "p", Name: "p", Root: dir, Worktrees: []state.Worktree{{ID: "w", Root: dir}}, Instances: map[string]state.Instance{
			id:    {ID: id, Service: "s", WorktreeID: "w", State: "stopped", RunID: "referenced", Preparation: &state.Intent{RunID: "preparing", OperationID: "op", Phase: "preparing"}},
			other: {ID: other, Service: "t", WorktreeID: "w", State: "stopped"},
		}})
	})
	if err != nil {
		t.Fatal(err)
	}
	create := func(run, instance string, record state.RunRecord, stamp time.Time) {
		t.Helper()
		err := store.WriteManifest(run, state.Manifest{RunID: run, InstanceID: instance, OperationID: "op"})
		if err != nil {
			t.Fatal(err)
		}
		if err = store.WriteRun(run, record); err != nil {
			t.Fatal(err)
		}
		runDir := filepath.Join(dir, "runs", run)
		if err = os.WriteFile(filepath.Join(runDir, "binary"), []byte("artifact"), 0600); err != nil {
			t.Fatal(err)
		}
		if err = os.Mkdir(filepath.Join(runDir, "logs"), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.Chtimes(filepath.Join(runDir, "manifest.json"), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-time.Hour)
	terminal := state.RunRecord{State: "stopped", Supervisor: dead}
	for n := 0; n < 8; n++ {
		create(fmt.Sprintf("run-%02d", n), id, terminal, base.Add(time.Duration(n)*time.Second))
	}
	for _, name := range []string{"referenced", "preparing"} {
		create(name, id, terminal, base.Add(-time.Hour))
	}
	create("other", other, terminal, base.Add(-time.Hour))
	self, err := procid.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	create("live", id, state.RunRecord{State: "running", Supervisor: self}, base.Add(-time.Hour))
	create("uncertain", id, state.RunRecord{State: "stopped", Supervisor: procid.Ref{PID: os.Getpid()}}, base.Add(-time.Hour))
	create("corrupt", id, terminal, base.Add(-time.Hour))
	if err = os.WriteFile(filepath.Join(dir, "runs", "corrupt", "manifest.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err = os.WriteFile(filepath.Join(outside, "keep"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(dir, "runs", "run-00", "outside-link")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(dir, "runs", "linked")); err != nil {
		t.Fatal(err)
	}
	runner := New(store, "")
	lock, err := store.LockInstances(id)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = runner.pruneRuns(id); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 8; n++ {
		runDir := filepath.Join(dir, "runs", fmt.Sprintf("run-%02d", n))
		_, err := os.Lstat(runDir)
		if n < 3 && !os.IsNotExist(err) {
			t.Fatalf("old run retained: %s %v", runDir, err)
		}
		if n >= 3 && err != nil {
			t.Fatalf("recent run removed: %s %v", runDir, err)
		}
	}
	for _, name := range []string{"referenced", "preparing", "other", "live", "uncertain", "corrupt", "linked"} {
		if _, err := os.Lstat(filepath.Join(dir, "runs", name)); err != nil {
			t.Fatalf("protected %s: %v", name, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(outside, "keep")); err != nil || string(b) != "secret" {
		t.Fatalf("symlink target changed: %v", err)
	}
}

func TestPruneFailureReturnsActualResult(t *testing.T) {
	dir := t.TempDir()
	store, err := state.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := "p/w/s"
	err = store.Update(func(reg *state.Registry) error {
		if err := reg.SetProject("p", state.Project{ID: "p", Name: "p", Root: dir, Worktrees: []state.Worktree{{ID: "w", Root: dir}}, Instances: map[string]state.Instance{id: {ID: id, Service: "s", WorktreeID: "w", State: "stopped"}}}); err != nil {
			return err
		}
		reg.Projects["bad"] = json.RawMessage(`{"id":"bad"}`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := New(store, "").Stop(context.Background(), &discovery.Project{ID: "p"}, &discovery.Worktree{ID: "w", Root: dir}, "s")
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "IO_ERROR" || result.InstanceID != id || result.State != "stopped" {
		t.Fatalf("result %+v error %v", result, err)
	}
}

func TestRetentionAfterSuccessfulRestarts(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rdr")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/rdr").CombinedOutput(); err != nil {
		t.Fatalf("binary: %v %s", err, out)
	}
	projectDir := filepath.Join(dir, "project")
	if err := os.Mkdir(projectDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "dev.toml"), []byte("version = 1\n[project]\nname = 'demo'\n[services.worker]\ncommand = 'sleep 60'\nstart_timeout = '3s'\nstop_timeout = '1s'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := discovery.Discover(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	w := &p.Worktrees[0]
	id := p.ID + "/" + w.ID + "/worker"
	store, err := state.New(filepath.Join(dir, "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: map[string]state.Instance{id: {ID: id, Service: "worker", WorktreeID: w.ID, State: "stopped"}}})
	}); err != nil {
		t.Fatal(err)
	}
	runner := New(store, binary)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		runner.Stop(ctx, p, w, "worker")
	}()
	if _, err = runner.Up(ctx, p, w, "worker"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		if _, err = runner.Restart(ctx, p, w, "worker"); err != nil {
			t.Fatalf("restart %d: %v", i, err)
		}
	}
	runs, err := os.ReadDir(filepath.Join(store.Root, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 6 {
		t.Fatalf("expected current run plus five completed, got %d", len(runs))
	}
}

func TestRetentionPrunesConfirmedFailedPreparations(t *testing.T) {
	dir := t.TempDir()
	store, err := state.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := "p/w/s"
	if err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject("p", state.Project{ID: "p", Name: "p", Root: dir, Worktrees: []state.Worktree{{ID: "w", Root: dir}}, Instances: map[string]state.Instance{id: {ID: id, Service: "s", WorktreeID: "w", State: "stopped"}}})
	}); err != nil {
		t.Fatal(err)
	}
	dead := deadSession(t)
	base := time.Now().Add(-time.Hour)
	for n := 0; n < 7; n++ {
		run := fmt.Sprintf("prep-%d", n)
		if err = store.WriteManifest(run, state.Manifest{RunID: run, InstanceID: id, OperationID: "op"}); err != nil {
			t.Fatal(err)
		}
		if err = store.WritePreparation(run, state.PreparationRecord{Child: dead}); err != nil {
			t.Fatal(err)
		}
		stamp := base.Add(time.Duration(n) * time.Second)
		if err = os.Chtimes(filepath.Join(dir, "runs", run, "manifest.json"), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := store.LockInstances(id)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = New(store, "").pruneRuns(id); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 7; n++ {
		_, err = os.Stat(filepath.Join(dir, "runs", fmt.Sprintf("prep-%d", n)))
		if n < 2 && !os.IsNotExist(err) {
			t.Fatalf("old preparation %d remains: %v", n, err)
		}
		if n >= 2 && err != nil {
			t.Fatalf("recent preparation %d removed: %v", n, err)
		}
	}
}
