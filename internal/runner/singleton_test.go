// Tests singleton planning, fixed-port handoff, and failure isolation.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestSingletonHandoff(t *testing.T) {
	service, err := os.ReadFile("../../examples/mini/service.py")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "rundevrun")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	for _, scenario := range []string{"handoff", "build-failure", "stop-failure", "start-failure", "parallel", "auto", "process", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			dir := canonicalTempDir(t)
			root, other := filepath.Join(dir, "main"), filepath.Join(dir, "other")
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			git := func(args ...string) {
				t.Helper()
				c := exec.Command("git", args...)
				c.Dir = root
				if out, err := c.CombinedOutput(); err != nil {
					t.Fatalf("git: %v %s", err, out)
				}
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			if err = listener.Close(); err != nil {
				t.Fatal(err)
			}
			cfg := fmt.Sprintf(`version = 1
[project]
name = 'demo'
[services.api]
command = 'python3 service.py catalog'
port = %d
singleton = true
health = { http = '/health' }
stop_timeout = '2s'
env = { NEXTAUTH_URL = 'http://localhost:%d' }
[services.worker]
command = 'sleep 60'
depends_on = ['api']
stop_timeout = '2s'
[services.extra]
command = 'sleep 60'
stop_timeout = '2s'
`, port, port)
			if scenario == "auto" {
				cfg = strings.Replace(cfg, fmt.Sprintf("port = %d", port), "port = 'auto'", 1)
			}
			if scenario == "process" {
				cfg = strings.Replace(cfg, "python3 service.py catalog", "sleep 60", 1)
				cfg = strings.Replace(cfg, fmt.Sprintf("port = %d\n", port), "", 1)
				cfg = strings.Replace(cfg, "health = { http = '/health' }\n", "", 1)
			}
			if scenario == "disabled" {
				cfg = strings.Replace(cfg, "singleton = true", "singleton = false", 1)
			}
			if scenario == "stop-failure" {
				cfg = strings.Replace(cfg, "singleton = true", "singleton = true\nhooks = { before_stop = 'test -f allow-stop' }", 1)
			}
			write := func(path, content string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(root, "rundevrun.toml"), cfg)
			write(filepath.Join(root, "service.py"), string(service))
			git("init", "-q")
			git("config", "user.name", "Test")
			git("config", "user.email", "test@example.invalid")
			git("add", "rundevrun.toml", "service.py")
			git("commit", "-qm", "initial")
			git("worktree", "add", "-qb", "other", other)
			if scenario == "build-failure" {
				write(filepath.Join(other, "rundevrun.toml"), strings.Replace(cfg, "singleton = true", "singleton = true\nbuild = 'false'", 1))
			}
			if scenario == "start-failure" {
				write(filepath.Join(other, "rundevrun.toml"), strings.Replace(cfg, "python3 service.py catalog", "false", 1))
			}
			p, err := discovery.Discover(root)
			if err != nil {
				t.Fatal(err)
			}
			a, err := discovery.Resolve(p, root)
			if err != nil {
				t.Fatal(err)
			}
			b, err := discovery.Resolve(p, other)
			if err != nil {
				t.Fatal(err)
			}
			store, err := state.New(filepath.Join(dir, "state"))
			if err != nil {
				t.Fatal(err)
			}
			if err = store.Update(func(reg *state.Registry) error {
				pr := state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Instances: map[string]state.Instance{}}
				for _, w := range []*discovery.Worktree{a, b} {
					pr.Worktrees = append(pr.Worktrees, state.Worktree{ID: w.ID, Root: w.Root})
					for name := range w.Config.Services {
						id := p.ID + "/" + w.ID + "/" + name
						pr.Instances[id] = state.Instance{ID: id, WorktreeID: w.ID, Service: name, State: "stopped"}
					}
				}
				return reg.SetProject(p.ID, pr)
			}); err != nil {
				t.Fatal(err)
			}
			r := New(store, binary)
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			t.Cleanup(func() {
				write(filepath.Join(root, "allow-stop"), "")
				r.RetryHooks = true
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, err := r.DownGraph(ctx, []Selection{{p, a, "api"}, {p, b, "api"}, {p, a, "extra"}, {p, b, "extra"}}, true)
				if err != nil {
					t.Errorf("cleanup: %v", err)
				}
			})
			if scenario == "parallel" {
				if _, err := r.PlanGraph(ctx, "up", []Selection{{p, a, "api"}, {p, b, "api"}}, false); graphErrorCode(err) != "BINDING_INVALID" {
					t.Fatalf("multiple targets: %v", err)
				}
				locks, err := r.lockServiceStarts([]PlannedInstance{{InstanceID: p.ID + "/" + a.ID + "/api", singleton: true}})
				if err != nil {
					t.Fatal(err)
				}
				_, err = r.Up(ctx, p, b, "api")
				locks.Close()
				if graphErrorCode(err) != "BUSY" {
					t.Fatalf("concurrent start: %v", err)
				}
				return
			}
			first, err := r.Up(ctx, p, a, "worker")
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(store.Root, "registry.json"))
			if err != nil {
				t.Fatal(err)
			}
			selections := []Selection{{p, b, "worker"}, {p, b, "extra"}}
			plan, err := r.PlanGraph(ctx, "up", selections, false)
			if scenario == "disabled" {
				if graphErrorCode(err) != "PORT_CONFLICT" {
					t.Fatalf("disabled singleton: %v", err)
				}
				return
			}
			if err != nil || plan.Switch == nil || len(plan.Switch.Mappings) != 1 || len(plan.Switch.OldStops) != 2 {
				t.Fatalf("plan: %+v %v", plan, err)
			}
			after, err := os.ReadFile(filepath.Join(store.Root, "registry.json"))
			if err != nil || string(before) != string(after) {
				t.Fatal("planning modified registry")
			}
			if _, err := json.Marshal(plan); err != nil {
				t.Fatal(err)
			}
			result, err := r.UpGraph(ctx, selections)
			failed := strings.HasSuffix(scenario, "failure")
			if (err != nil) != failed {
				t.Fatalf("handoff: %+v %v", result, err)
			}
			if len(result.Steps) == 0 || result.OperationID == "" {
				t.Fatalf("missing handoff details: %+v", result)
			}
			reg, err := store.Read()
			if err != nil {
				t.Fatal(err)
			}
			pr, err := reg.Project(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			old := pr.Instances[p.ID+"/"+a.ID+"/api"]
			next := pr.Instances[p.ID+"/"+b.ID+"/api"]
			oldState, err := store.Inspect(old)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "build-failure" || scenario == "stop-failure" {
				if !liveState(oldState) || next.RunID != "" {
					t.Fatalf("failed preparation/stop changed active instance: %s %+v", oldState, next)
				}
				return
			}
			if oldState != "stopped" {
				t.Fatalf("source still active: %s", oldState)
			}
			if scenario == "start-failure" {
				if result.Status != "partial" {
					t.Fatalf("failed target did not report partial handoff: %+v", result)
				}
				return
			}
			manifest, err := store.ReadManifest(next.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "auto" && scenario != "process" && manifest.Launch.Port != port {
				t.Fatalf("port changed: %d", manifest.Launch.Port)
			}
			if manifest.StopContract.Env["NEXTAUTH_URL"] != fmt.Sprintf("http://localhost:%d", port) {
				t.Fatal("auth URL changed")
			}
			consumer := pr.Instances[first.InstanceID]
			if consumer.RunID == first.RunID || consumer.EffectiveBindings["api"] != next.ID {
				t.Fatalf("consumer not rebound: %+v", consumer)
			}
			again, err := r.Up(ctx, p, b, "api")
			if err != nil || again.Status != "unchanged" || again.RunID != next.RunID {
				t.Fatalf("repeat up: %+v %v", again, err)
			}
			var coded *Error
			if _, err := r.PlanGraph(ctx, "up", []Selection{{p, a, "api"}, {p, b, "api"}}, false); !errors.As(err, &coded) || coded.Code != "BINDING_INVALID" {
				t.Fatalf("active and new target accepted together: %v", err)
			}
			if scenario == "handoff" {
				if _, err := r.UpGraph(ctx, []Selection{{p, a, "api"}, {p, a, "worker"}}); err != nil {
					t.Fatalf("handoff with a selected live consumer: %v", err)
				}
			}
		})
	}
}
