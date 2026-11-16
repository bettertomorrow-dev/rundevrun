//go:build linux

// Tests runner lifecycle behavior on Linux.
package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
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

func TestLifecycleProcess(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rdr")
	cmd := exec.Command("go", "build", "-o", binary, "../../cmd/rdr")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build binary: %v: %s", err, out)
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
	store, err := state.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: map[string]state.Instance{id: {ID: id, Service: "worker", WorktreeID: w.ID, State: "stopped"}}})
	}); err != nil {
		t.Fatal(err)
	}
	runner := New(store, binary)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	before, err := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := runner.Plan(ctx, "up", p, w, "worker")
	if err != nil || plan.InstanceID != id {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	after, err := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if err != nil || string(before) != string(after) {
		t.Fatalf("plan mutated registry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Root, "runs")); !os.IsNotExist(err) {
		t.Fatalf("plan created runs: %v", err)
	}
	first, err := runner.Up(ctx, p, w, "worker")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		runner.Stop(ctx, p, w, "worker")
	}()
	again, err := runner.Up(ctx, p, w, "worker")
	if err != nil || again.RunID != first.RunID || again.Status != "unchanged" {
		t.Fatalf("repeat: %+v %v", again, err)
	}
	restarted, err := runner.Restart(ctx, p, w, "worker")
	if err != nil || restarted.RunID == first.RunID {
		t.Fatalf("restart: %+v %v", restarted, err)
	}
	if err := os.Remove(filepath.Join(projectDir, "dev.toml")); err != nil {
		t.Fatal(err)
	}
	record, err := store.ReadRun(restarted.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(record.Supervisor.PID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	stopped, err := runner.Stop(ctx, p, w, "worker")
	if err != nil || stopped.State != "stopped" {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
}

func TestFixedPortRestart(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	dir := t.TempDir()
	binary := filepath.Join(dir, "rdr")
	cmd := exec.Command("go", "build", "-o", binary, "../../cmd/rdr")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("binary: %v %s", err, out)
	}
	projectDir := filepath.Join(dir, "project")
	if err = os.Mkdir(projectDir, 0700); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("version = 1\n[project]\nname = 'demo'\n[services.web]\ncommand = 'python3 -m http.server {port} --bind 127.0.0.1'\nport = %d\nstart_timeout = '5s'\nstop_timeout = '1s'\n", port)
	if err = os.WriteFile(filepath.Join(projectDir, "dev.toml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := discovery.Discover(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	w := &p.Worktrees[0]
	id := p.ID + "/" + w.ID + "/web"
	store, err := state.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: map[string]state.Instance{id: {ID: id, Service: "web", WorktreeID: w.ID, State: "stopped"}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := New(store, binary)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	first, err := runner.Up(ctx, p, w, "web")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		runner.Stop(ctx, p, w, "web")
	}()
	configPath := filepath.Join(projectDir, "dev.toml")
	failing := content + "build = 'exit 17'\n"
	if err := os.WriteFile(configPath, []byte(failing), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Restart(ctx, p, w, "web"); err == nil {
		t.Fatal("failed build accepted")
	}
	reg, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reg.Project(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Instances[id].RunID != first.RunID {
		t.Fatal("old run replaced after failed build")
	}
	record, err := store.ReadRun(first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := procid.Check(record.Child); err != nil || !ok {
		t.Fatalf("old process lost: %v %v", ok, err)
	}
	if err := os.WriteFile(configPath, []byte(content+"health = { http = '/' }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	next, err := runner.Restart(ctx, p, w, "web")
	if err != nil {
		t.Fatal(err)
	}
	if next.State != "ready" {
		t.Fatalf("health did not become ready: %+v", next)
	}
	if first.URLs["port"] != next.URLs["port"] || first.RunID == next.RunID {
		t.Fatalf("first %+v next %+v", first, next)
	}
	if _, err = runner.Stop(ctx, p, w, "web"); err != nil {
		t.Fatal(err)
	}
	stalled := fmt.Sprintf("version = 1\n[project]\nname = 'demo'\n[services.web]\ncommand = 'sleep 60'\nport = %d\nstart_timeout = '300ms'\nstop_timeout = '1s'\n", port)
	if err = os.WriteFile(configPath, []byte(stalled), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = runner.Up(ctx, p, w, "web"); err == nil {
		t.Fatal("readiness timeout accepted")
	}
	reg, err = store.Read()
	if err != nil {
		t.Fatal(err)
	}
	saved, err = reg.Project(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Instances[id].State != "stopped" {
		t.Fatalf("timeout did not clean up: %+v", saved.Instances[id])
	}
}

func TestStopFailsClosedAfterSupervisorDisappears(t *testing.T) {
	dir := t.TempDir()
	store, err := state.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := "p/w/s"
	err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject("p", state.Project{ID: "p", Name: "p", Root: dir, Worktrees: []state.Worktree{{ID: "w", Root: dir}}, Instances: map[string]state.Instance{id: {ID: id, Service: "s", WorktreeID: "w", State: "running", RunID: "run", Intent: &state.Intent{RunID: "run", OperationID: "op", Phase: "started"}}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.WriteManifest("run", state.Manifest{RunID: "run", InstanceID: id, Launch: state.LaunchContract{StopTimeout: "1s"}}); err != nil {
		t.Fatal(err)
	}
	ref, err := procid.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	ref.Identity += "foreign"
	if err = store.WriteRun("run", state.RunRecord{Claim: "accepted", Supervisor: ref, State: "running"}); err != nil {
		t.Fatal(err)
	}
	_, err = New(store, "").Stop(context.Background(), &discovery.Project{ID: "p"}, &discovery.Worktree{ID: "w", Root: dir}, "s")
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "OWNERSHIP_UNKNOWN" {
		t.Fatalf("expected unknown ownership, got %v", err)
	}
}
