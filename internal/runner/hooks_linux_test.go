//go:build linux

// Tests runner hooks behavior on Linux.
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestHookLifecycle(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rundevrun")
	c := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun")
	if b, e := c.CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	root := filepath.Join(dir, "project")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	for _, d := range []string{"old", "new"} {
		if e := os.Mkdir(filepath.Join(root, d), 0700); e != nil {
			t.Fatal(e)
		}
	}
	cfg := `version = 1
[project]
name = 'demo'
[services.app]
command = 'sleep 60'
cwd = 'old'
env = { VALUE = 'old' }
hooks = { before_start = 'echo before_start:$VALUE:$RDR_REASON >> ../events', after_start = 'echo after_start:$VALUE:$RDR_REASON >> ../events', before_stop = 'echo before_stop:$VALUE:$RDR_REASON >> ../events', after_stop = 'echo after_stop:$VALUE:$RDR_REASON >> ../events' }
`
	path := filepath.Join(root, "rundevrun.toml")
	if e := os.WriteFile(path, []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	p, e := discovery.Discover(root)
	if e != nil {
		t.Fatal(e)
	}
	w := &p.Worktrees[0]
	id := p.ID + "/" + w.ID + "/app"
	store, e := state.New(filepath.Join(dir, "state"))
	if e != nil {
		t.Fatal(e)
	}
	if e = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: map[string]state.Instance{id: {ID: id, Service: "app", WorktreeID: w.ID, State: "stopped"}}})
	}); e != nil {
		t.Fatal(e)
	}
	r := New(store, binary)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = r.Stop(c, p, w, "app")
		cancel()
	}()
	if _, e = r.Up(ctx, p, w, "app"); e != nil {
		t.Fatal(e)
	}
	updated := strings.Replace(cfg, "VALUE = 'old'", "VALUE = 'new'", 1)
	updated = strings.Replace(updated, "cwd = 'old'", "cwd = 'new'", 1)
	if e = os.WriteFile(path, []byte(updated), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Restart(ctx, p, w, "app"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Stop(ctx, p, w, "app"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Restart(ctx, p, w, "app"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Stop(ctx, p, w, "app"); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(filepath.Join(root, "events"))
	if e != nil {
		t.Fatal(e)
	}
	want := "before_start:old:up\nafter_start:old:up\nbefore_stop:old:restart\nafter_stop:old:restart\nbefore_start:new:restart\nafter_start:new:restart\nbefore_stop:new:down\nafter_stop:new:down\nbefore_start:new:restart\nafter_start:new:restart\nbefore_stop:new:down\nafter_stop:new:down\n"
	if string(data) != want {
		t.Fatalf("events: %q want %q", data, want)
	}
}

func TestHookFailures(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "rundevrun")
	if b, e := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun").CombinedOutput(); e != nil {
		t.Fatalf("build %v %s", e, b)
	}
	for _, name := range []string{"before_start", "after_start", "before_stop", "after_stop"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := "version = 1\n[project]\nname = 'demo'\n[services.app]\ncommand = 'sleep 60'\nhooks = { " + name + " = 'echo hit >> hits; exit 1' }\n"
			if e := os.WriteFile(filepath.Join(dir, "rundevrun.toml"), []byte(cfg), 0600); e != nil {
				t.Fatal(e)
			}
			p, e := discovery.Discover(dir)
			if e != nil {
				t.Fatal(e)
			}
			w := &p.Worktrees[0]
			id := p.ID + "/" + w.ID + "/app"
			store, e := state.New(filepath.Join(t.TempDir(), "state"))
			if e != nil {
				t.Fatal(e)
			}
			if e = store.Update(func(reg *state.Registry) error {
				return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: map[string]state.Instance{id: {ID: id, Service: "app", WorktreeID: w.ID, State: "stopped"}}})
			}); e != nil {
				t.Fatal(e)
			}
			r := New(store, binary)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			defer func() {
				reg, _ := store.Read()
				pr, e := reg.Project(p.ID)
				if e == nil {
					inst := pr.Instances[id]
					if inst.RunID != "" {
						_, _ = r.Stop(ctx, p, w, "app")
						_ = r.stopLocked(ctx, p.ID, id, inst.RunID)
					}
				}
			}()
			_, e = r.Up(ctx, p, w, "app")
			if strings.HasSuffix(name, "_start") {
				if e == nil {
					t.Fatal("start hook succeeded")
				}
				reg, _ := store.Read()
				pr, _ := reg.Project(p.ID)
				observed, err := store.Inspect(pr.Instances[id])
				if err != nil {
					t.Fatal(err)
				}
				if name == "after_start" && observed != "running" {
					t.Fatalf("after_start lost process: %s", observed)
				}
				if name == "before_start" && observed != "stopped" {
					t.Fatalf("before_start started process: %s", observed)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			_, e = r.Stop(ctx, p, w, "app")
			if e == nil {
				t.Fatal("stop hook succeeded")
			}
			reg, _ := store.Read()
			pr, _ := reg.Project(p.ID)
			observed, err := store.Inspect(pr.Instances[id])
			if err != nil {
				t.Fatal(err)
			}
			if name == "before_stop" && observed != "running" {
				t.Fatalf("before_stop stopped process: %s", observed)
			}
			if name == "after_stop" && observed != "stopped" {
				t.Fatalf("after_stop left process: %s", observed)
			}
			if _, e = r.Stop(ctx, p, w, "app"); e == nil {
				t.Fatal("stop hook replayed without opt-in")
			}
			if name == "after_stop" {
				if _, e = r.Up(ctx, p, w, "app"); e == nil {
					t.Fatal("after_stop failure allowed new start")
				}
			}

			hits, e := os.ReadFile(filepath.Join(dir, "hits"))
			if e != nil || string(hits) != "hit\n" {
				t.Fatalf("hook replay: %q %v", hits, e)
			}
			changed := strings.Replace(cfg, "echo hit >> hits; exit 1", "echo new >> hits; exit 1", 1)
			if e = os.WriteFile(filepath.Join(dir, "rundevrun.toml"), []byte(changed), 0600); e != nil {
				t.Fatal(e)
			}
			r.RetryHooks = true
			if _, e = r.Stop(ctx, p, w, "app"); e == nil {
				t.Fatal("failed saved hook retry succeeded")
			}
			hits, e = os.ReadFile(filepath.Join(dir, "hits"))
			if e != nil || string(hits) != "hit\nhit\n" {
				t.Fatalf("retry used changed contract: %q %v", hits, e)
			}
		})
	}
}

func TestInterruptedHookDoesNotReplay(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rundevrun")
	if b, e := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun").CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	root := filepath.Join(dir, "project")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := `version = 1
[project]
name = 'demo'
[services.app]
command = 'sleep 60'
hooks = { before_start = 'echo hit >> hits; sleep 60 & echo $! > child; wait' }
`
	if e := os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	runtime := filepath.Join(dir, "state")
	cli := func(args ...string) *exec.Cmd {
		c := exec.Command(binary, args...)
		c.Dir = root
		c.Env = append(os.Environ(), "RDR_HOME="+runtime)
		return c
	}
	if out, e := cli("register", ".", "--json").CombinedOutput(); e != nil {
		t.Fatalf("register: %v %s", e, out)
	}
	up := cli("up", "app", "--json")
	if e := up.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if up.Process != nil {
			_ = up.Process.Kill()
			_, _ = up.Process.Wait()
		}
	}()
	childFile := filepath.Join(root, "child")
	var data []byte
	var e error
	for n := 0; n < 200; n++ {
		data, e = os.ReadFile(childFile)
		if e == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if e != nil {
		t.Fatal("hook did not start")
	}
	pid, e := strconv.Atoi(strings.TrimSpace(string(data)))
	if e != nil {
		t.Fatal(e)
	}
	if e = up.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = up.Wait()
	up.Process = nil
	if out, e := cli("down", "app", "--json").CombinedOutput(); e != nil {
		t.Fatalf("recover down: %v %s", e, out)
	}
	hits, e := os.ReadFile(filepath.Join(root, "hits"))
	if e != nil || string(hits) != "hit\n" {
		t.Fatalf("hook replayed: %q %v", hits, e)
	}
	for n := 0; n < 100; n++ {
		if e = syscall.Kill(pid, 0); errors.Is(e, syscall.ESRCH) {
			return
		}
		b, readErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if readErr != nil || strings.Contains(string(b), ") Z ") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("hook descendant survived recovery")
}

func TestHookTimeoutCleansDescendants(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rundevrun")
	if b, e := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun").CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	root := filepath.Join(dir, "project")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := `version = 1
[project]
name = 'demo'
[services.app]
command = 'sleep 60'
hook_timeout = '300ms'
hooks = { before_start = 'sleep 60 & echo $! > child; wait' }
`
	if e := os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	cli := func(args ...string) *exec.Cmd {
		c := exec.Command(binary, args...)
		c.Dir = root
		c.Env = append(os.Environ(), "RDR_HOME="+filepath.Join(dir, "state"))
		return c
	}
	if out, e := cli("register", ".", "--json").CombinedOutput(); e != nil {
		t.Fatalf("register: %v %s", e, out)
	}
	if out, e := cli("up", "app", "--json").CombinedOutput(); e == nil {
		t.Fatalf("hook unexpectedly succeeded: %s", out)
	}
	data, e := os.ReadFile(filepath.Join(root, "child"))
	if e != nil {
		t.Fatal(e)
	}
	pid, e := strconv.Atoi(strings.TrimSpace(string(data)))
	if e != nil {
		t.Fatal(e)
	}
	for n := 0; n < 100; n++ {
		if e = syscall.Kill(pid, 0); errors.Is(e, syscall.ESRCH) {
			return
		}
		b, readErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if readErr != nil || strings.Contains(string(b), ") Z ") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("hook descendant survived timeout")
}

func TestHookOperationIDSharedAcrossBatch(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rundevrun")
	if b, e := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun").CombinedOutput(); e != nil {
		t.Fatalf("build %v %s", e, b)
	}
	root := filepath.Join(dir, "project")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := `version = 1
[project]
name = 'demo'
[services.a]
command = 'sleep 60'
hooks = { before_start = 'echo $RDR_OPERATION_ID > a-up', before_stop = 'echo $RDR_OPERATION_ID > a-down' }
[services.b]
command = 'sleep 60'
hooks = { before_start = 'echo $RDR_OPERATION_ID > b-up', before_stop = 'echo $RDR_OPERATION_ID > b-down' }
`
	if e := os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	p, e := discovery.Discover(root)
	if e != nil {
		t.Fatal(e)
	}
	w := &p.Worktrees[0]
	store, e := state.New(filepath.Join(dir, "state"))
	if e != nil {
		t.Fatal(e)
	}
	instances := map[string]state.Instance{}
	for _, name := range []string{"a", "b"} {
		id := p.ID + "/" + w.ID + "/" + name
		instances[id] = state.Instance{ID: id, Service: name, WorktreeID: w.ID, State: "stopped"}
	}
	if e = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: instances})
	}); e != nil {
		t.Fatal(e)
	}
	r := New(store, binary)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sels := []Selection{{p, w, "a"}, {p, w, "b"}}
	defer r.DownGraph(context.Background(), sels, false)
	if _, e = r.UpGraph(ctx, sels); e != nil {
		t.Fatal(e)
	}
	if _, e = r.DownGraph(ctx, sels, false); e != nil {
		t.Fatal(e)
	}
	for _, phase := range []string{"up", "down"} {
		a, e := os.ReadFile(filepath.Join(root, "a-"+phase))
		if e != nil {
			t.Fatal(e)
		}
		b, e := os.ReadFile(filepath.Join(root, "b-"+phase))
		if e != nil || len(a) == 0 || string(a) != string(b) {
			t.Fatalf("batch operation id %s: %q %q %v", phase, a, b, e)
		}
	}
}

func TestAfterStartFailureBlocksConsumerUntilRestart(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rundevrun")
	if b, e := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun").CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	root := filepath.Join(dir, "project")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := `version = 1
[project]
name = 'demo'
[services.api]
command = 'sleep 60'
hooks = { after_start = 'exit 1' }
[services.app]
command = 'touch started; sleep 60'
depends_on = ['api']
`
	path := filepath.Join(root, "rundevrun.toml")
	if e := os.WriteFile(path, []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	p, e := discovery.Discover(root)
	if e != nil {
		t.Fatal(e)
	}
	w := &p.Worktrees[0]
	store, e := state.New(filepath.Join(dir, "state"))
	if e != nil {
		t.Fatal(e)
	}
	instances := map[string]state.Instance{}
	for _, name := range []string{"api", "app"} {
		id := p.ID + "/" + w.ID + "/" + name
		instances[id] = state.Instance{ID: id, Service: name, WorktreeID: w.ID, State: "stopped"}
	}
	if e = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: instances})
	}); e != nil {
		t.Fatal(e)
	}
	r := New(store, binary)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	defer func() { _, _ = r.Stop(ctx, p, w, "app"); _, _ = r.Stop(ctx, p, w, "api") }()
	if _, e = r.Up(ctx, p, w, "app"); e == nil {
		t.Fatal("after_start succeeded")
	}
	if _, e = r.Up(ctx, p, w, "app"); e == nil {
		t.Fatal("failed dependency admitted consumer")
	}
	if _, e = os.Stat(filepath.Join(root, "started")); !os.IsNotExist(e) {
		t.Fatalf("consumer started: %v", e)
	}
	cfg = strings.Replace(cfg, "hooks = { after_start = 'exit 1' }\n", "", 1)
	if e = os.WriteFile(path, []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Restart(ctx, p, w, "api"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Up(ctx, p, w, "app"); e != nil {
		t.Fatal(e)
	}
}

func TestInterruptedStopHookRequiresRetry(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rundevrun")
	if b, e := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun").CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	root := filepath.Join(dir, "project")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := `version = 1
[project]
name = 'demo'
[services.app]
command = 'sleep 60'
hooks = { before_stop = 'echo hit >> hits; sleep 60 & echo $! > child; wait' }
`
	if e := os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	runtime := filepath.Join(dir, "state")
	cli := func(args ...string) *exec.Cmd {
		c := exec.Command(binary, args...)
		c.Dir = root
		c.Env = append(os.Environ(), "RDR_HOME="+runtime)
		return c
	}
	if out, e := cli("register", ".", "--json").CombinedOutput(); e != nil {
		t.Fatalf("register: %v %s", e, out)
	}
	if out, e := cli("up", "app", "--json").CombinedOutput(); e != nil {
		t.Fatalf("up: %v %s", e, out)
	}
	p, e := discovery.Discover(root)
	if e != nil {
		t.Fatal(e)
	}
	w := p.Worktrees[0]
	id := p.ID + "/" + w.ID + "/app"
	store := &state.Store{Root: runtime}
	defer func() {
		reg, e := store.Read()
		if e == nil {
			pr, e := reg.Project(p.ID)
			if e == nil {
				i := pr.Instances[id]
				if i.RunID != "" {
					c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					_ = New(store, binary).stopLocked(c, p.ID, id, i.RunID)
					cancel()
				}
			}
		}
	}()
	down := cli("down", "app", "--json")
	if e = down.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if down.Process != nil {
			_ = down.Process.Kill()
			_, _ = down.Process.Wait()
		}
	}()
	for n := 0; n < 200; n++ {
		if _, e = os.Stat(filepath.Join(root, "child")); e == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if e != nil {
		t.Fatal("hook did not start")
	}
	if e = down.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = down.Wait()
	down.Process = nil
	if out, e := cli("down", "app", "--json").CombinedOutput(); e == nil {
		t.Fatalf("interrupted hook accepted: %s", out)
	}
	hits, e := os.ReadFile(filepath.Join(root, "hits"))
	if e != nil || string(hits) != "hit\n" {
		t.Fatalf("interrupted hook replayed: %q %v", hits, e)
	}
	reg, e := store.Read()
	if e != nil {
		t.Fatal(e)
	}
	pr, e := reg.Project(p.ID)
	if e != nil {
		t.Fatal(e)
	}
	i := pr.Instances[id]
	observed, e := store.Inspect(i)
	if e != nil || observed != "running" || i.Hook == nil || i.Hook.Phase != "interrupted" {
		t.Fatalf("state=%s hook=%+v err=%v", observed, i.Hook, e)
	}
}

func TestStopHookRetryRunsOnce(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rundevrun")
	if b, e := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun").CombinedOutput(); e != nil {
		t.Fatalf("build %v %s", e, b)
	}
	root := filepath.Join(dir, "project")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := `version = 1
[project]
name = 'demo'
[services.app]
command = 'sleep 60'
hooks = { before_stop = 'if [ -f flag ]; then echo pass >> hits; else touch flag; echo fail >> hits; exit 1; fi' }
`
	if e := os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	p, e := discovery.Discover(root)
	if e != nil {
		t.Fatal(e)
	}
	w := &p.Worktrees[0]
	id := p.ID + "/" + w.ID + "/app"
	store, e := state.New(filepath.Join(dir, "state"))
	if e != nil {
		t.Fatal(e)
	}
	if e = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: map[string]state.Instance{id: {ID: id, Service: "app", WorktreeID: w.ID, State: "stopped"}}})
	}); e != nil {
		t.Fatal(e)
	}
	r := New(store, binary)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	defer func() {
		reg, e := store.Read()
		if e == nil {
			pr, e := reg.Project(p.ID)
			if e == nil {
				i := pr.Instances[id]
				if i.RunID != "" {
					_ = r.stopLocked(context.Background(), p.ID, id, i.RunID)
				}
			}
		}
	}()
	if _, e = r.Up(ctx, p, w, "app"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Stop(ctx, p, w, "app"); e == nil {
		t.Fatal("expected hook failure")
	}
	r.RetryHooks = true
	if _, e = r.Stop(ctx, p, w, "app"); e != nil {
		t.Fatalf("retry: %v", e)
	}
	hits, e := os.ReadFile(filepath.Join(root, "hits"))
	if e != nil || string(hits) != "fail\npass\n" {
		t.Fatalf("hook replayed: %q %v", hits, e)
	}
}
