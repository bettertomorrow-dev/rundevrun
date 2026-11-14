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

func TestPlanSwitchSharedConsumer(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main")
	other := filepath.Join(dir, "other")
	third := filepath.Join(dir, "third")
	if e := os.Mkdir(main, 0700); e != nil {
		t.Fatal(e)
	}
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = main
		if b, e := c.CombinedOutput(); e != nil {
			t.Fatalf("git: %v %s", e, b)
		}
	}
	git("init", "-q")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	cfg := `version = 1
[project]
name = 'demo'
[services.api]
command = 'sleep 60'
port = 'auto'
[services.app]
command = 'sleep 60'
depends_on = ['api']
env = { API = '{dependencies.api.url}' }
`
	if e := os.WriteFile(filepath.Join(main, "dev.toml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	git("add", "dev.toml")
	git("commit", "-qm", "initial")
	git("worktree", "add", "-qb", "other", other)
	git("worktree", "add", "-qb", "third", third)
	p, e := discovery.Discover(main)
	if e != nil {
		t.Fatal(e)
	}
	var a, b, c *discovery.Worktree
	for i := range p.Worktrees {
		w := &p.Worktrees[i]
		if w.Root == main {
			a = w
		}
		if w.Root == other {
			b = w
		}
		if w.Root == third {
			c = w
		}
	}
	if a == nil || b == nil || c == nil {
		t.Fatal("missing worktree")
	}
	store, e := state.New(filepath.Join(dir, "state"))
	if e != nil {
		t.Fatal(e)
	}
	instances := map[string]state.Instance{}
	var trees []state.Worktree
	for _, w := range []*discovery.Worktree{a, b, c} {
		trees = append(trees, state.Worktree{ID: w.ID, Root: w.Root})
		for _, svc := range []string{"api", "app"} {
			id := p.ID + "/" + w.ID + "/" + svc
			instances[id] = state.Instance{ID: id, Service: svc, WorktreeID: w.ID, State: "stopped"}
		}
	}
	api := p.ID + "/" + a.ID + "/api"
	app := p.ID + "/" + a.ID + "/app"
	target := p.ID + "/" + b.ID + "/api"
	ref, e := procid.Observe(os.Getpid())
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range []struct {
		id, run  string
		bindings map[string]string
		address  map[string]string
	}{{api, "api-run", nil, map[string]string{"host": "127.0.0.1", "port": "40111", "direct_url": "http://127.0.0.1:40111", "url": "http://127.0.0.1:40111"}}, {app, "app-run", map[string]string{"api": api}, map[string]string{"host": "127.0.0.1"}}} {
		i := instances[v.id]
		i.State = "running"
		i.RunID = v.run
		instances[v.id] = i
		if e = store.WriteManifest(v.run, state.Manifest{RunID: v.run, InstanceID: v.id, Addresses: v.address, EffectiveBindings: v.bindings}); e != nil {
			t.Fatal(e)
		}
		if e = store.WriteRun(v.run, state.RunRecord{State: "running", Child: ref}); e != nil {
			t.Fatal(e)
		}
	}
	if e = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: trees, Instances: instances})
	}); e != nil {
		t.Fatal(e)
	}
	publication := state.Publication{ID: "pub-switch", ProjectID: p.ID, RootID: app, RootRunID: "app-run", Mode: "live", Phase: "active", Cloudflared: "/bin/true", Owner: ref, Salt: make([]byte, 16), Verifier: make([]byte, 32), SigningKey: make([]byte, 32), RoutedRuns: map[string]string{api: "api-run"}}
	if e = store.WritePublications(state.PublicationRegistry{Version: 1, Publications: map[string]state.Publication{publication.ID: publication}}); e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if e != nil {
		t.Fatal(e)
	}
	r := New(store, "")
	plan, e := r.PlanSwitch(context.Background(), []SwitchMapping{{Selection{p, a, "api"}, Selection{p, b, "api"}}})
	if e != nil {
		t.Fatal(e)
	}
	if len(plan.Publications) != 1 || plan.Publications[0] != publication.ID {
		t.Fatalf("switch publication plan: %+v", plan.Publications)
	}
	if len(plan.OldStops) != 2 || plan.OldStops[0].InstanceID != app || len(plan.NewStarts) != 2 || plan.NewStarts[0].InstanceID != target || plan.NewStarts[1].InstanceID != app || plan.NewStarts[1].Bindings["api"] != target {
		t.Fatalf("unexpected graph: %+v", plan)
	}
	stepMap := map[string]SwitchStep{}
	for _, step := range plan.Steps {
		if _, exists := stepMap[step.ID]; exists {
			t.Fatalf("duplicate step %s", step.ID)
		}
		stepMap[step.ID] = step
	}
	marks := map[string]int{}
	var visit func(string)
	visit = func(id string) {
		t.Helper()
		if marks[id] == 1 {
			t.Fatalf("step cycle at %s", id)
		}
		if marks[id] == 2 {
			return
		}
		step, ok := stepMap[id]
		if !ok {
			t.Fatalf("unknown step %s", id)
		}
		marks[id] = 1
		for _, dep := range step.DependsOn {
			visit(dep)
		}
		marks[id] = 2
	}
	for id := range stepMap {
		visit(id)
	}
	after, e := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if e != nil || string(before) != string(after) {
		t.Fatal("planner wrote registry")
	}
	bjson, _ := json.Marshal(plan)
	if len(bjson) == 0 {
		t.Fatal("empty plan")
	}
	invalid := [][]SwitchMapping{
		{{Selection{p, a, "api"}, Selection{p, a, "api"}}},
		{{Selection{p, a, "api"}, Selection{p, b, "api"}}, {Selection{p, a, "api"}, Selection{p, c, "api"}}},
		{{Selection{p, a, "api"}, Selection{p, b, "api"}}, {Selection{p, c, "api"}, Selection{p, b, "api"}}},
		{{Selection{p, a, "api"}, Selection{p, b, "app"}}},
		{{Selection{p, a, "missing"}, Selection{p, b, "missing"}}},
		{{Selection{p, b, "api"}, Selection{p, c, "api"}}},
	}
	for index, mappings := range invalid {
		if _, err := r.PlanSwitch(context.Background(), mappings); err == nil {
			t.Fatalf("invalid case %d accepted", index)
		}
	}
	targetApp := p.ID + "/" + b.ID + "/app"
	both, e := r.PlanSwitch(context.Background(), []SwitchMapping{{Selection{p, a, "api"}, Selection{p, b, "api"}}, {Selection{p, a, "app"}, Selection{p, b, "app"}}})
	if e != nil {
		t.Fatal(e)
	}
	if len(both.OldStops) != 2 || len(both.NewStarts) != 2 || both.NewStarts[1].InstanceID != targetApp || both.NewStarts[1].Bindings["api"] != target {
		t.Fatalf("mapped consumer: %+v", both)
	}
	if _, e = r.PlanSwitch(context.Background(), []SwitchMapping{{Selection{p, a, "api"}, Selection{p, b, "api"}}, {Selection{p, b, "api"}, Selection{p, a, "api"}}}); e == nil {
		t.Fatal("overlap accepted")
	}
	thirdAPI := p.ID + "/" + c.ID + "/api"
	thirdApp := p.ID + "/" + c.ID + "/app"
	if e = store.Update(func(reg *state.Registry) error {
		project, e := reg.Project(p.ID)
		if e != nil {
			return e
		}
		i := project.Instances[targetApp]
		i.DesiredBindings = map[string]string{"api": thirdAPI}
		project.Instances[targetApp] = i
		stopped := project.Instances[thirdApp]
		stopped.DesiredBindings = map[string]string{"api": api}
		project.Instances[thirdApp] = stopped
		return reg.SetProject(p.ID, project)
	}); e != nil {
		t.Fatal(e)
	}
	overridePlan, e := r.PlanSwitch(context.Background(), []SwitchMapping{{Selection{p, a, "api"}, Selection{p, b, "api"}}, {Selection{p, a, "app"}, Selection{p, b, "app"}}})
	if e != nil {
		t.Fatal(e)
	}
	foundOverride, foundStopped := false, false
	for _, node := range overridePlan.NewStarts {
		if node.InstanceID == targetApp && node.Bindings["api"] == thirdAPI {
			foundOverride = true
		}
		if node.InstanceID == thirdApp {
			t.Fatal("stopped consumer was started")
		}
	}
	for _, rewrite := range overridePlan.BindingRewrites {
		if rewrite.ConsumerID == thirdApp && rewrite.From == api && rewrite.To == target {
			foundStopped = true
		}
	}
	if !foundOverride || !foundStopped {
		t.Fatalf("override/rewrite: %+v", overridePlan)
	}
	cycleConfig := strings.Replace(cfg, "port = 'auto'\n[services.app]", "port = 'auto'\ndepends_on = ['app']\n[services.app]", 1)
	cycleConfig = strings.Replace(cycleConfig, "depends_on = ['api']\nenv = { API = '{dependencies.api.url}' }\n", "", 1)
	if e = os.WriteFile(filepath.Join(c.Root, "dev.toml"), []byte(cycleConfig), 0600); e != nil {
		t.Fatal(e)
	}
	if e = store.Update(func(reg *state.Registry) error {
		project, e := reg.Project(p.ID)
		if e != nil {
			return e
		}
		i := project.Instances[thirdAPI]
		i.DesiredBindings = map[string]string{"app": targetApp}
		project.Instances[thirdAPI] = i
		return reg.SetProject(p.ID, project)
	}); e != nil {
		t.Fatal(e)
	}
	if _, e = r.PlanSwitch(context.Background(), []SwitchMapping{{Selection{p, a, "api"}, Selection{p, b, "api"}}, {Selection{p, a, "app"}, Selection{p, b, "app"}}}); e == nil {
		t.Fatal("cross-worktree cycle accepted")
	}
	var cycle *Error
	if !errors.As(e, &cycle) || cycle.Code != "CYCLE" {
		t.Fatalf("expected CYCLE: %v", e)
	}
	if e = os.WriteFile(filepath.Join(c.Root, "dev.toml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	if e = store.Update(func(reg *state.Registry) error {
		project, e := reg.Project(p.ID)
		if e != nil {
			return e
		}
		i := project.Instances[thirdAPI]
		i.DesiredBindings = nil
		project.Instances[thirdAPI] = i
		return reg.SetProject(p.ID, project)
	}); e != nil {
		t.Fatal(e)
	}
	if e = store.Update(func(reg *state.Registry) error {
		project, e := reg.Project(p.ID)
		if e != nil {
			return e
		}
		i := project.Instances[targetApp]
		i.DesiredBindings["api"] = api
		project.Instances[targetApp] = i
		return reg.SetProject(p.ID, project)
	}); e != nil {
		t.Fatal(e)
	}
	if _, e = r.PlanSwitch(context.Background(), []SwitchMapping{{Selection{p, a, "api"}, Selection{p, b, "api"}}, {Selection{p, a, "app"}, Selection{p, b, "app"}}}); e == nil {
		t.Fatal("target override to stopping source accepted")
	}
	if e = store.Update(func(reg *state.Registry) error {
		project, e := reg.Project(p.ID)
		if e != nil {
			return e
		}
		i := project.Instances[app]
		i.DesiredBindings = map[string]string{"api": target}
		project.Instances[app] = i
		return reg.SetProject(p.ID, project)
	}); e != nil {
		t.Fatal(e)
	}
	if _, e = r.PlanSwitch(context.Background(), []SwitchMapping{{Selection{p, a, "api"}, Selection{p, b, "api"}}}); e == nil {
		t.Fatal("pending desired binding accepted")
	}

}

func TestPlanSwitchRejectsTransitiveDefaultToStoppingSource(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main")
	other := filepath.Join(dir, "other")
	if e := os.Mkdir(main, 0700); e != nil {
		t.Fatal(e)
	}
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = main
		if b, e := c.CombinedOutput(); e != nil {
			t.Fatalf("git: %v %s", e, b)
		}
	}
	git("init", "-q")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@example.invalid")
	base := `version = 1
[project]
name = 'demo'
[services.api]
command = 'sleep 60'
[services.helper]
command = 'sleep 60'
depends_on = ['api']
`
	if e := os.WriteFile(filepath.Join(main, "dev.toml"), []byte(base), 0600); e != nil {
		t.Fatal(e)
	}
	git("add", "dev.toml")
	git("commit", "-qm", "initial")
	git("worktree", "add", "-qb", "other", other)
	targetConfig := `version = 1
[project]
name = 'demo'
[services.api]
command = 'sleep 60'
depends_on = ['helper']
[services.helper]
command = 'sleep 60'
`
	if e := os.WriteFile(filepath.Join(other, "dev.toml"), []byte(targetConfig), 0600); e != nil {
		t.Fatal(e)
	}
	p, e := discovery.Discover(main)
	if e != nil {
		t.Fatal(e)
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
		t.Fatal("missing worktree")
	}
	store, e := state.New(filepath.Join(dir, "state"))
	if e != nil {
		t.Fatal(e)
	}
	instances := map[string]state.Instance{}
	var trees []state.Worktree
	for _, w := range []*discovery.Worktree{a, b} {
		trees = append(trees, state.Worktree{ID: w.ID, Root: w.Root})
		for _, name := range []string{"api", "helper"} {
			id := p.ID + "/" + w.ID + "/" + name
			instances[id] = state.Instance{ID: id, Service: name, WorktreeID: w.ID, State: "stopped"}
		}
	}
	apiA := p.ID + "/" + a.ID + "/api"
	apiB := p.ID + "/" + b.ID + "/api"
	helperA := p.ID + "/" + a.ID + "/helper"
	i := instances[apiA]
	i.State = "running"
	i.RunID = "active"
	instances[apiA] = i
	i = instances[apiB]
	i.DesiredBindings = map[string]string{"helper": helperA}
	instances[apiB] = i
	ref, e := procid.Observe(os.Getpid())
	if e != nil {
		t.Fatal(e)
	}
	if e = store.WriteManifest("active", state.Manifest{RunID: "active", InstanceID: apiA, Addresses: map[string]string{"host": "127.0.0.1"}}); e != nil {
		t.Fatal(e)
	}
	if e = store.WriteRun("active", state.RunRecord{State: "running", Child: ref}); e != nil {
		t.Fatal(e)
	}
	if e = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: trees, Instances: instances})
	}); e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if e != nil {
		t.Fatal(e)
	}
	plan, e := New(store, "").PlanSwitch(context.Background(), []SwitchMapping{{Selection{p, a, "api"}, Selection{p, b, "api"}}})
	var coded *Error
	if !errors.As(e, &coded) || coded.Code != "BINDING_INVALID" {
		t.Fatalf("expected BINDING_INVALID: %+v %v", plan, e)
	}
	after, e := os.ReadFile(filepath.Join(store.Root, "registry.json"))
	if e != nil || string(before) != string(after) {
		t.Fatal("rejected plan modified registry")
	}
	fixed := strings.Replace(base, "depends_on = ['api']\n", "", 1)
	if e = os.WriteFile(filepath.Join(main, "dev.toml"), []byte(fixed), 0600); e != nil {
		t.Fatal(e)
	}
	plan, e = New(store, "").PlanSwitch(context.Background(), []SwitchMapping{{Selection{p, a, "api"}, Selection{p, b, "api"}}})
	if e != nil {
		t.Fatal(e)
	}
	steps := map[string]SwitchStep{}
	for _, step := range plan.Steps {
		steps[step.ID] = step
	}
	contains := func(values []string, expected string) bool {
		for _, v := range values {
			if v == expected {
				return true
			}
		}
		return false
	}
	if !contains(steps["prepare:"+apiB].DependsOn, "start:"+helperA) || !contains(steps["stop:"+apiA].DependsOn, "prepare:"+apiB) || !contains(steps["stop:"+apiA].DependsOn, "prepare:"+helperA) {
		t.Fatalf("incorrect preparation ordering: %+v", plan.Steps)
	}
	marks := map[string]int{}
	var visit func(string)
	visit = func(id string) {
		t.Helper()
		if marks[id] == 1 {
			t.Fatalf("cycle in executable step graph at %s", id)
		}
		if marks[id] == 2 {
			return
		}
		step, ok := steps[id]
		if !ok {
			t.Fatalf("missing step %s", id)
		}
		marks[id] = 1
		for _, dep := range step.DependsOn {
			visit(dep)
		}
		marks[id] = 2
	}
	for id := range steps {
		visit(id)
	}
}
