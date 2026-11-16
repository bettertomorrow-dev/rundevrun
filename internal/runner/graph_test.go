// Tests runner graph behavior.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestGraphPlanReadOnlyAndBindings(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, "project")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := `version = 1
[project]
name = 'demo'
[services.api]
command = 'echo api'
port = 'auto'
[services.worker]
command = 'echo worker'
depends_on = ['api']
env = { API = '{dependencies.api.direct_url}', SECRET = 'private-value' }
`
	if err := os.WriteFile(filepath.Join(project, "dev.toml"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := discovery.Discover(project)
	if err != nil {
		t.Fatal(err)
	}
	w := &p.Worktrees[0]
	store, err := state.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	instances := map[string]state.Instance{}
	for _, name := range []string{"api", "worker"} {
		id := p.ID + "/" + w.ID + "/" + name
		ids[name] = id
		instances[id] = state.Instance{ID: id, Service: name, WorktreeID: w.ID, State: "stopped"}
	}
	if err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: instances})
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	r := New(store, "")
	selection := Selection{p, w, "worker"}
	plan, err := r.PlanGraph(context.Background(), "up", []Selection{selection}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Instances) != 2 || plan.Instances[0].InstanceID != ids["api"] || plan.Instances[1].Bindings["api"] != ids["api"] {
		t.Fatalf("bad order or bindings: %+v", plan)
	}
	if plan.Instances[1].env["RDR_DEP_API_DIRECT_URL"] != "http://127.0.0.1:20000" || plan.Instances[1].env["API"] != "http://127.0.0.1:20000" {
		t.Fatalf("bad environment: %+v", plan.Instances[1].env)
	}
	if _, ok := plan.Instances[1].env["RDR_PORT"]; ok {
		t.Fatal("no-port service received port")
	}
	b, _ := json.Marshal(plan)
	if strings.Contains(string(b), "private-value") {
		t.Fatal("plan exposed secret")
	}
	after, err := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("planner modified registry")
	}
	if _, err = os.Stat(filepath.Join(store.Root, "runs")); !os.IsNotExist(err) {
		t.Fatalf("planner created runtime files: %v", err)
	}
	bound, err := r.Bind(context.Background(), selection, "api", Selection{p, w, "api"})
	if err != nil || bound.TargetID != ids["api"] || bound.RestartRequired {
		t.Fatalf("bind: %+v %v", bound, err)
	}
	fixed := strings.Replace(cfg, "port = 'auto'", "port = 23456", 1)
	fixed = strings.Replace(fixed, "command = 'echo worker'", "command = 'echo worker'\nport = 23456", 1)
	if err = os.WriteFile(filepath.Join(project, "dev.toml"), []byte(fixed), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = r.PlanGraph(context.Background(), "up", []Selection{selection}, false)
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.Code != "PORT_CONFLICT" {
		t.Fatalf("expected fixed port conflict: %v", err)
	}
	if err = store.Update(func(reg *state.Registry) error {
		pr, e := reg.Project(p.ID)
		if e != nil {
			return e
		}
		i := pr.Instances[ids["worker"]]
		i.Reservations = map[string]int{"held": 23456}
		pr.Instances[i.ID] = i
		return reg.SetProject(p.ID, pr)
	}); err != nil {
		t.Fatal(err)
	}
	_, err = r.PlanGraph(context.Background(), "up", []Selection{{p, w, "api"}}, false)
	if !errors.As(err, &conflict) || conflict.Code != "PORT_CONFLICT" {
		t.Fatalf("expected reserved fixed port conflict: %v", err)
	}
}

func TestGraphCrossWorktreeBinding(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main")
	other := filepath.Join(dir, "other")
	if err := os.Mkdir(main, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = main
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "Test")
	cfg := `version = 1
[project]
name = 'demo'
[services.api]
command = 'echo api'
port = 'auto'
[services.worker]
command = 'echo worker'
depends_on = ['api']
`
	if err := os.WriteFile(filepath.Join(main, "dev.toml"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "dev.toml")
	git("commit", "-qm", "initial")
	git("worktree", "add", "-qb", "other", other)
	p, err := discovery.Discover(main)
	if err != nil {
		t.Fatal(err)
	}
	var a, b *discovery.Worktree
	for i := range p.Worktrees {
		w := &p.Worktrees[i]
		if w.Root == main {
			a = w
		}
		if w.Root == other {
			b = w
		}
	}
	if a == nil || b == nil {
		t.Fatal("missing worktrees")
	}
	store, err := state.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	instances := map[string]state.Instance{}
	var worktrees []state.Worktree
	for _, w := range []*discovery.Worktree{a, b} {
		worktrees = append(worktrees, state.Worktree{ID: w.ID, Root: w.Root})
		for _, name := range []string{"worker", "api"} {
			id := p.ID + "/" + w.ID + "/" + name
			instances[id] = state.Instance{ID: id, Service: name, WorktreeID: w.ID, State: "stopped"}
		}
	}
	if err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: worktrees, Instances: instances})
	}); err != nil {
		t.Fatal(err)
	}
	r := New(store, "")
	consumerID := p.ID + "/" + a.ID + "/worker"
	lock, err := store.LockInstances(consumerID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Bind(context.Background(), Selection{p, a, "worker"}, "api", Selection{p, b, "api"})
	var busy *Error
	if !errors.As(err, &busy) || busy.Code != "BUSY" {
		t.Fatalf("expected BUSY: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if err != nil || string(after) != string(before) {
		t.Fatal("busy bind changed registry")
	}
	lock.Close()
	consumer := Selection{p, a, "worker"}
	target := Selection{p, b, "api"}
	bound, err := r.Bind(context.Background(), consumer, "api", target)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.PlanGraph(context.Background(), "up", []Selection{consumer, {p, b, "worker"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Instances) != 3 || plan.Instances[0].InstanceID != bound.TargetID || plan.Instances[1].Bindings["api"] != bound.TargetID || plan.Instances[2].Bindings["api"] != bound.TargetID {
		t.Fatalf("shared binding: %+v", plan)
	}
	consumerID = p.ID + "/" + a.ID + "/worker"
	oldTarget := p.ID + "/" + a.ID + "/api"
	ref, err := procid.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.WriteManifest("active", state.Manifest{RunID: "active", InstanceID: consumerID, Digest: "old", EffectiveBindings: map[string]string{"api": oldTarget}}); err != nil {
		t.Fatal(err)
	}
	if err = store.WriteRun("active", state.RunRecord{Child: ref, State: "running"}); err != nil {
		t.Fatal(err)
	}
	if err = store.Update(func(reg *state.Registry) error {
		project, err := reg.Project(p.ID)
		if err != nil {
			return err
		}
		i := project.Instances[consumerID]
		i.RunID = "active"
		i.State = "running"
		project.Instances[consumerID] = i
		return reg.SetProject(p.ID, project)
	}); err != nil {
		t.Fatal(err)
	}
	bound, err = r.Bind(context.Background(), consumer, "api", target)
	if err != nil || !bound.RestartRequired {
		t.Fatalf("pending bind: %+v %v", bound, err)
	}
	restart, err := r.PlanGraph(context.Background(), "restart", []Selection{consumer}, false)
	if err != nil || len(restart.Instances) != 2 || restart.Instances[1].Bindings["api"] != bound.TargetID {
		t.Fatalf("restart must use desired binding: %+v %v", restart, err)
	}

	if _, err = r.PlanGraph(context.Background(), "down", []Selection{{p, a, "api"}}, false); err == nil {
		t.Fatal("down ignored effective live consumer")
	}
	down, err := r.PlanGraph(context.Background(), "down", []Selection{{p, a, "api"}}, true)
	if err != nil || len(down.Instances) != 2 || down.Instances[0].InstanceID != consumerID {
		t.Fatalf("cascade: %+v %v", down, err)
	}

	if err = store.WriteManifest("preparing", state.Manifest{RunID: "preparing", InstanceID: consumerID, EffectiveBindings: map[string]string{"api": bound.TargetID}}); err != nil {
		t.Fatal(err)
	}
	if err = store.Update(func(reg *state.Registry) error {
		project, e := reg.Project(p.ID)
		if e != nil {
			return e
		}
		i := project.Instances[consumerID]
		i.Preparation = &state.Intent{RunID: "preparing", OperationID: "test", Phase: "preparing"}
		project.Instances[consumerID] = i
		return reg.SetProject(p.ID, project)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = r.PlanGraph(context.Background(), "down", []Selection{{p, b, "api"}}, false); err == nil {
		t.Fatal("down ignored preparing consumer")
	}
	pending, err := r.PlanGraph(context.Background(), "down", []Selection{{p, b, "api"}}, true)
	if err != nil || len(pending.Instances) != 2 || pending.Instances[0].InstanceID != consumerID {
		t.Fatalf("preparation cascade: %+v %v", pending, err)
	}
	if err = store.Update(func(reg *state.Registry) error {
		project, e := reg.Project(p.ID)
		if e != nil {
			return e
		}
		i := project.Instances[consumerID]
		i.Preparation = nil
		project.Instances[consumerID] = i
		return reg.SetProject(p.ID, project)
	}); err != nil {
		t.Fatal(err)
	}

	withURL := strings.Replace(cfg, "depends_on = ['api']", "depends_on = ['api']\nenv = { API = '{dependencies.api.url}' }", 1)
	if err = os.WriteFile(filepath.Join(main, "dev.toml"), []byte(withURL), 0600); err != nil {
		t.Fatal(err)
	}
	noPort := strings.Replace(cfg, "port = 'auto'\n", "", 1)
	if err = os.WriteFile(filepath.Join(other, "dev.toml"), []byte(noPort), 0600); err != nil {
		t.Fatal(err)
	}
	before, err = os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.PlanBind(context.Background(), consumer, "api", target); err == nil {
		t.Fatal("non-HTTP binding accepted")
	}
	if _, err = r.Bind(context.Background(), consumer, "api", target); err == nil {
		t.Fatal("non-HTTP binding committed")
	}
	after, err = os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("invalid binding changed registry")
	}
	apiID := p.ID + "/" + a.ID + "/api"
	if err = store.WriteManifest("api-active", state.Manifest{RunID: "api-active", InstanceID: apiID, Digest: "old-api", Addresses: map[string]string{"host": "127.0.0.1", "port": "45678", "direct_url": "http://127.0.0.1:45678", "url": "http://127.0.0.1:45678"}}); err != nil {
		t.Fatal(err)
	}
	if err = store.WriteRun("api-active", state.RunRecord{Child: ref, State: "running"}); err != nil {
		t.Fatal(err)
	}
	if err = store.Update(func(reg *state.Registry) error {
		project, e := reg.Project(p.ID)
		if e != nil {
			return e
		}
		i := project.Instances[apiID]
		i.RunID = "api-active"
		i.State = "running"
		project.Instances[apiID] = i
		return reg.SetProject(p.ID, project)
	}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(main, "dev.toml"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	active, err := r.PlanGraph(context.Background(), "up", []Selection{{p, a, "api"}}, false)
	if err != nil || len(active.Instances) != 1 || active.Instances[0].Addresses["port"] != "45678" || active.Instances[0].Digest != "old-api" {
		t.Fatalf("active manifest lost: %+v %v", active, err)
	}

}

func TestConcurrentBindCycle(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main")
	other := filepath.Join(dir, "other")
	if err := os.Mkdir(main, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = main
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "Test")
	base := "version = 1\n[project]\nname = 'demo'\n[services.alpha]\ncommand = 'true'\n[services.beta]\ncommand = 'true'\n"
	if err := os.WriteFile(filepath.Join(main, "dev.toml"), []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "dev.toml")
	git("commit", "-qm", "init")
	git("worktree", "add", "-qb", "other", other)
	ca := strings.Replace(base, "[services.beta]", "depends_on = ['beta']\n[services.beta]", 1)
	cb := base + "depends_on = ['alpha']\n"
	if err := os.WriteFile(filepath.Join(main, "dev.toml"), []byte(ca), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "dev.toml"), []byte(cb), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := discovery.Discover(main)
	if err != nil {
		t.Fatal(err)
	}
	var a, b *discovery.Worktree
	for i := range p.Worktrees {
		w := &p.Worktrees[i]
		if w.Root == main {
			a = w
		}
		if w.Root == other {
			b = w
		}
	}
	if a == nil || b == nil {
		t.Fatal("missing worktrees")
	}
	store, err := state.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	instances := map[string]state.Instance{}
	var worktrees []state.Worktree
	for _, w := range []*discovery.Worktree{a, b} {
		worktrees = append(worktrees, state.Worktree{ID: w.ID, Root: w.Root})
		for _, name := range []string{"alpha", "beta"} {
			id := p.ID + "/" + w.ID + "/" + name
			instances[id] = state.Instance{ID: id, Service: name, WorktreeID: w.ID, State: "stopped"}
		}
	}
	if err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: worktrees, Instances: instances})
	}); err != nil {
		t.Fatal(err)
	}
	r := New(store, "")
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, e := r.Bind(context.Background(), Selection{p, a, "alpha"}, "beta", Selection{p, b, "beta"})
		results <- e
	}()
	go func() {
		<-start
		_, e := r.Bind(context.Background(), Selection{p, b, "beta"}, "alpha", Selection{p, a, "alpha"})
		results <- e
	}()
	close(start)
	first, second := <-results, <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("expected one success: %v, %v", first, second)
	}
	failure := first
	if failure == nil {
		failure = second
	}
	var coded *Error
	if !errors.As(failure, &coded) || coded.Code != "CYCLE" {
		t.Fatalf("expected CYCLE: %v", failure)
	}
	if _, err = r.PlanGraph(context.Background(), "up", []Selection{{p, a, "alpha"}, {p, b, "beta"}}, false); err != nil {
		t.Fatalf("committed graph invalid: %v", err)
	}
}
