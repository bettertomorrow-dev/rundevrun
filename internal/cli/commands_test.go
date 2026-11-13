package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func invoke(t *testing.T, args ...string) (envelope, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(context.Background(), args, &out, &errOut)
	var e envelope
	if err := json.Unmarshal(out.Bytes(), &e); err != nil {
		t.Fatalf("invalid JSON %q, stderr=%q: %v", out.String(), errOut.String(), err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", &errOut)
	}
	return e, code
}

func writeProject(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	// Execution would create a marker, including during discovery or dry-run.
	if err := os.WriteFile(filepath.Join(root, "dev.toml"), []byte(`version = 1
[project]
name = "example"
[services.worker]
command = "touch executed; sleep 60"
build = "touch executed"
`), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRegistrationDiscoveryAndStoppedStatus(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtime := filepath.Join(base, "state")
	writeProject(t, root)
	t.Setenv("RDR_HOME", runtime)
	t.Chdir(root)
	for _, args := range [][]string{{"config", "check", "--json"}, {"register", ".", "--dry-run", "--json"}} {
		if e, code := invoke(t, args...); code != 0 || e.Status != "ok" {
			t.Fatalf("%v: exit=%d result=%+v", args, code, e)
		}
		if _, err := os.Stat(runtime); !os.IsNotExist(err) {
			t.Fatalf("read-only operation created runtime: %v", err)
		}
	}
	if e, code := invoke(t, "register", ".", "--json"); code != 0 || len(e.Results) == 0 {
		t.Fatalf("registration: exit=%d result=%+v", code, e)
	}
	t.Chdir(base)
	var instanceID string
	for _, args := range [][]string{{"services", "--project", "example", "--json"}, {"status", "--project", "example", "--json"}, {"ls", "--json"}} {
		e, code := invoke(t, args...)
		if code != 0 || len(e.Results) != 1 {
			t.Fatalf("%v: exit=%d result=%+v", args, code, e)
		}
		i := e.Results[0].(map[string]any)
		if i["service"] != "worker" || i["state"] != "stopped" {
			t.Fatal(i)
		}
		if instanceID != "" && instanceID != i["instance_id"] {
			t.Fatalf("unstable ID: %v", i)
		}
		instanceID = i["instance_id"].(string)
	}
	if _, err := os.Stat(filepath.Join(root, "executed")); !os.IsNotExist(err) {
		t.Fatalf("discovery executed project code: %v", err)
	}
	clone := filepath.Join(base, "other-clone")
	writeProject(t, clone)
	if e, code := invoke(t, "register", clone, "--json"); code != 0 {
		t.Fatalf("second registration: %d %+v", code, e)
	}
	e, code := invoke(t, "services", "--project", "example", "--json")
	if code != 2 || len(e.Errors) != 1 || e.Errors[0].Code != "AMBIGUOUS" || len(e.Errors[0].Candidates) != 2 {
		t.Fatalf("ambiguous project: %d %+v", code, e)
	}
}

func TestUsageAndVersionErrorsAreJSON(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RDR_HOME", filepath.Join(root, "state"))
	t.Chdir(root)
	if err := os.WriteFile("dev.toml", []byte("version = 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	e, code := invoke(t, "config", "check", "--json")
	if code != 2 || len(e.Errors) != 1 || e.Errors[0].Code != "VERSION_UNSUPPORTED" {
		t.Fatalf("version: %d %+v", code, e)
	}
	e, code = invoke(t, "--unknown", "--json")
	if code != 2 || len(e.Errors) != 1 || e.Errors[0].Code != "USAGE" {
		t.Fatalf("usage: %d %+v", code, e)
	}
}

func TestLifecyclePlanAndSingleServiceGrammar(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	t.Setenv("RDR_HOME", filepath.Join(base, "state"))
	writeProject(t, root)
	t.Chdir(root)
	if e, code := invoke(t, "register", ".", "--json"); code != 0 || e.Status != "ok" {
		t.Fatalf("register: %d %+v", code, e)
	}
	e, code := invoke(t, "up", "worker", "--dry-run", "--json")
	if code != 0 || e.Plan == nil || len(e.Results) != 0 {
		t.Fatalf("dry run: %d %+v", code, e)
	}
	if _, code = invoke(t, "up", "--dry-run", "--json"); code != 2 {
		t.Fatalf("missing service exit=%d", code)
	}
	if e, code = invoke(t, "down", "--all", "--json"); code != 0 || len(e.Results) != 1 {
		t.Fatalf("global down no-op: exit=%d result=%+v", code, e)
	}
}

func TestDownUsesRegisteredDescriptorWithoutConfig(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	t.Setenv("RDR_HOME", filepath.Join(base, "state"))
	writeProject(t, root)
	t.Chdir(root)
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatalf("register exit=%d", code)
	}
	if err := os.Remove(filepath.Join(root, "dev.toml")); err != nil {
		t.Fatal(err)
	}
	e, code := invoke(t, "down", "worker", "--json")
	if code != 0 || len(e.Results) != 1 {
		t.Fatalf("down without config: %d %+v", code, e)
	}
}

func TestLifecycleSelectionFromSubdirectoryAndOutsideProject(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	t.Setenv("RDR_HOME", filepath.Join(base, "state"))
	writeProject(t, root)
	t.Chdir(root)
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatal(code)
	}
	subdir := filepath.Join(root, "sub", "nested")
	if err := os.MkdirAll(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)
	if e, code := invoke(t, "up", "worker", "--dry-run", "--json"); code != 0 {
		t.Fatalf("subdirectory selection: %d %+v", code, e)
	}
	t.Chdir(base)
	if e, code := invoke(t, "up", "worker", "--project", "example", "--dry-run", "--json"); code != 2 || e.Errors[0].Code != "USAGE" {
		t.Fatalf("outside project inferred worktree: %d %+v", code, e)
	}
	if e, code := invoke(t, "up", "worker", "--project", "example", "--worktree", root, "--dry-run", "--json"); code != 0 {
		t.Fatalf("explicit outside selection: %d %+v", code, e)
	}
}

func TestBindDryRunUsesSharedValidationWithoutWritingState(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtime := filepath.Join(base, "state")
	t.Setenv("RDR_HOME", runtime)
	writeProject(t, root)
	t.Chdir(root)
	if err := os.WriteFile("dev.toml", []byte(`version=1
[project]
name="example"
[services.api]
command="touch executed"
port="auto"
[services.worker]
command="touch executed"
depends_on=["api"]
env={API_URL="{dependencies.api.direct_url}"}
`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatal(code)
	}
	before, err := os.ReadFile(filepath.Join(runtime, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	e, code := invoke(t, "bind", "worker", "api", "--to-worktree", root, "--dry-run", "--json")
	if code != 0 || e.Plan == nil {
		t.Fatalf("bind plan: %d %+v", code, e)
	}
	after, err := os.ReadFile(filepath.Join(runtime, "registry.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("bind dry-run wrote state: %v", err)
	}
	if _, err := os.Stat("executed"); !os.IsNotExist(err) {
		t.Fatal("bind executed project code")
	}
	if e, code := invoke(t, "bind", "worker", "api", "--to-worktree", root, "--json"); code != 0 || len(e.Results) != 1 {
		t.Fatalf("bind: %d %+v", code, e)
	}
	if e, code := invoke(t, "up", "worker", "--dry-run", "--json"); code != 0 || e.Plan == nil {
		t.Fatalf("graph plan: %d %+v", code, e)
	}
}

func TestLifecycleUsesCurrentBranchLabels(t *testing.T) {
	root := t.TempDir()
	t.Setenv("RDR_HOME", filepath.Join(t.TempDir(), "state"))
	writeProject(t, root)
	t.Chdir(root)
	git := func(args ...string) {
		t.Helper()
		prefix := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "commit.gpgSign=false", "-c", "user.name=Fixture", "-c", "user.email=example@example.com"}
		if out, err := exec.Command("git", append(prefix, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", out, err)
		}
	}
	git("init", "-q", "-b", "before")
	git("commit", "--allow-empty", "-qm", "fixture")
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatal(code)
	}
	git("branch", "-m", "after")
	if e, code := invoke(t, "up", "worker", "--worktree", "after", "--dry-run", "--json"); code != 0 {
		t.Fatalf("fresh branch selector: %d %+v", code, e)
	}
	if e, code := invoke(t, "up", "worker", "--worktree", "before", "--dry-run", "--json"); code != 2 {
		t.Fatalf("stale branch selected a different checkout: %d %+v", code, e)
	}
}

func TestConfigDriftAgainstRunManifest(t *testing.T) {
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manifest := state.Manifest{RunID: "run-1", InstanceID: "p/w/worker", Digest: "old-digest"}
	if err := store.WriteManifest("run-1", manifest); err != nil {
		t.Fatal(err)
	}
	instance := state.Instance{ID: manifest.InstanceID, RunID: manifest.RunID, State: "running"}
	changed, restart := configDrift(store, instance, "new-digest", &envelope{})
	if !changed || !restart {
		t.Fatalf("changed config: changed=%t restart=%t", changed, restart)
	}
	changed, restart = configDrift(store, instance, "old-digest", &envelope{})
	if changed || restart {
		t.Fatalf("unchanged config: changed=%t restart=%t", changed, restart)
	}
	instance.State = "unhealthy"
	changed, restart = configDrift(store, instance, "new-digest", &envelope{})
	if !changed || !restart {
		t.Fatal("unhealthy live run lost configuration drift")
	}
}

func TestStatusKeepsProcessStateWhenConfigIsInvalid(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	t.Setenv("RDR_HOME", filepath.Join(base, "state"))
	writeProject(t, root)
	t.Chdir(root)
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatalf("register exit=%d", code)
	}
	store := &state.Store{Root: filepath.Join(base, "state")}
	if err := store.Update(func(r *state.Registry) error {
		for id := range r.Projects {
			p, err := r.Project(id)
			if err != nil {
				return err
			}
			for key, i := range p.Instances {
				i.Preparation = &state.Intent{RunID: "pending-run", OperationID: "op", Phase: "preparing"}
				p.Instances[key] = i
			}
			return r.SetProject(id, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dev.toml"), []byte("invalid = ["), 0600); err != nil {
		t.Fatal(err)
	}
	e, code := invoke(t, "status", "--project", "example", "--json")
	if code != 0 || len(e.Results) != 1 {
		t.Fatalf("status exit=%d result=%+v", code, e)
	}
	item := e.Results[0].(map[string]any)
	if item["state"] != "stopped" || item["config_error"] == nil {
		t.Fatalf("status lost process state/config error: %#v", item)
	}
	prep := item["preparation"].(map[string]any)
	if prep["run_id"] != "pending-run" || prep["phase"] != "preparing" || prep["build_log_path"] == nil || item["env"] != nil {
		t.Fatalf("invalid preparation display: %#v", item)
	}
	if e, code := invoke(t, "status", "--project", "example", "--worktree", root, "--json"); code != 0 || len(e.Results) != 1 {
		t.Fatalf("scoped status required valid config: %d %+v", code, e)
	}
}

func TestStatusReportsUnreadableRunAsError(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtime := filepath.Join(base, "state")
	t.Setenv("RDR_HOME", runtime)
	writeProject(t, root)
	t.Chdir(root)
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatal(code)
	}
	s := &state.Store{Root: runtime}
	if err := s.Update(func(r *state.Registry) error {
		for id := range r.Projects {
			p, err := r.Project(id)
			if err != nil {
				return err
			}
			for key, i := range p.Instances {
				i.RunID, i.State = "missing-run", "starting"
				p.Instances[key] = i
			}
			if err := r.SetProject(id, p); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e, code := invoke(t, "status", "--json")
	if code != 5 || len(e.Errors) != 1 || e.Errors[0].Code != "OWNERSHIP_UNKNOWN" {
		t.Fatalf("unverified status reported success: %d %+v", code, e)
	}
	i := e.Results[0].(map[string]any)
	if i["state_unknown"] != true || i["state"] != nil {
		t.Fatalf("invented process state: %+v", i)
	}
}

func TestStatusIncludesAddressesAndLogPathsWithoutPrivateEnvironment(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtime := filepath.Join(base, "state")
	t.Setenv("RDR_HOME", runtime)
	writeProject(t, root)
	t.Chdir(root)
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatal(code)
	}
	s := &state.Store{Root: runtime}
	var instanceID string
	if err := s.Update(func(r *state.Registry) error {
		for id := range r.Projects {
			p, err := r.Project(id)
			if err != nil {
				return err
			}
			for key, i := range p.Instances {
				i.RunID = "saved-run"
				instanceID = key
				p.Instances[key] = i
			}
			return r.SetProject(id, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteManifest("saved-run", state.Manifest{RunID: "saved-run", InstanceID: instanceID, Addresses: map[string]string{"direct_url": "http://127.0.0.1:32123", "url": "https://worker.example.com"}, StopContract: state.StopContract{Env: map[string]string{"TOKEN": "DO_NOT_DISCLOSE"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteRun("saved-run", state.RunRecord{State: "stopped"}); err != nil {
		t.Fatal(err)
	}
	e, code := invoke(t, "status", "--json")
	if code != 0 || len(e.Results) != 1 {
		t.Fatalf("%d %+v", code, e)
	}
	item := e.Results[0].(map[string]any)
	if item["direct_url"] != "http://127.0.0.1:32123" || item["url"] != "https://worker.example.com" || item["log_paths"] == nil {
		t.Fatal(item)
	}
	encoded, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("DO_NOT_DISCLOSE")) || item["env"] != nil {
		t.Fatal("status disclosed private environment")
	}
}

func writeDependencyProject(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dev.toml"), []byte(`version = 1
[project]
name = "example"
[services.api]
command = "touch executed"
protocol = "http"
port = "auto"
[services.worker]
command = "touch executed"
protocol = "process"
depends_on = ["api"]
env = { API_URL = "{dependencies.api.direct_url}" }
`), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestGraphDryRunsSupportListsDefaultAndGlobalDown(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtime := filepath.Join(base, "state")
	t.Setenv("RDR_HOME", runtime)
	writeDependencyProject(t, root)
	t.Chdir(root)
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatalf("register exit=%d", code)
	}
	store := &state.Store{Root: runtime}
	otherRoot := filepath.Join(base, "other-worktree")
	writeDependencyProject(t, otherRoot)
	if err := store.Update(func(reg *state.Registry) error {
		for id := range reg.Projects {
			p, err := reg.Project(id)
			if err != nil {
				return err
			}
			other := state.Worktree{ID: "w_other", Root: otherRoot}
			p.Worktrees = append(p.Worktrees, other)
			for _, service := range []string{"api", "worker"} {
				id := p.ID + "/" + other.ID + "/" + service
				p.Instances[id] = state.Instance{ID: id, Service: service, WorktreeID: other.ID, State: "stopped"}
			}
			if err := reg.SetProject(id, p); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(runtime, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		args  []string
		count int
	}{
		{[]string{"up", "api", "worker", "--dry-run", "--json"}, 2},
		{[]string{"restart", "api", "worker", "--dry-run", "--json"}, 2},
		{[]string{"down", "api", "worker", "--cascade", "--dry-run", "--json"}, 2},
		{[]string{"down", "--dry-run", "--json"}, 2},
		{[]string{"down", "--all", "--dry-run", "--json"}, 4},
	} {
		e, code := invoke(t, tc.args...)
		if code != 0 || e.Plan == nil {
			t.Fatalf("%v: exit=%d result=%+v", tc.args, code, e)
		}
		encoded, err := json.Marshal(e.Plan)
		if err != nil {
			t.Fatal(err)
		}
		var plan map[string]any
		if err := json.Unmarshal(encoded, &plan); err != nil {
			t.Fatal(err)
		}
		instances := plan["instances"].([]any)
		if len(instances) != tc.count {
			t.Fatalf("%v selected %d instances, want %d: %s", tc.args, len(instances), tc.count, encoded)
		}
	}
	after, err := os.ReadFile(filepath.Join(runtime, "registry.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("dry-run changed registry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runtime, "runs")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created run directory: %v", err)
	}
}

func TestGlobalDownRejectsMixedScopesWithoutWritingRegistry(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtime := filepath.Join(base, "state")
	t.Setenv("RDR_HOME", runtime)
	writeProject(t, root)
	t.Chdir(root)
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatalf("register exit=%d", code)
	}
	before, err := os.ReadFile(filepath.Join(runtime, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"down", "--all", "--project", "example", "--json"},
		{"down", "--all", "--worktree", root, "--json"},
		{"down", "worker", "--all", "--json"},
		{"up", "worker", "--cascade", "--dry-run", "--json"},
	} {
		e, code := invoke(t, args...)
		if code != 2 || len(e.Errors) != 1 || e.Errors[0].Code != "USAGE" {
			t.Fatalf("%v: exit=%d result=%+v", args, code, e)
		}
	}
	after, err := os.ReadFile(filepath.Join(runtime, "registry.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected scope changed registry: %v", err)
	}
}

func TestStatusShowsExactDesiredAndEffectiveBindings(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtime := filepath.Join(base, "state")
	otherRoot := filepath.Join(base, "other-worktree")
	t.Setenv("RDR_HOME", runtime)
	writeDependencyProject(t, root)
	writeDependencyProject(t, otherRoot)
	t.Chdir(root)
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatalf("register exit=%d", code)
	}
	store := &state.Store{Root: runtime}
	reg, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	var initial state.Project
	for id := range reg.Projects {
		initial, err = reg.Project(id)
		if err != nil {
			t.Fatal(err)
		}
	}
	defaultAPIID := initial.ID + "/" + initial.Worktrees[0].ID + "/api"
	services, code := invoke(t, "services", "--project", "example", "--worktree", root, "--json")
	if code != 0 {
		t.Fatalf("services: exit=%d result=%+v", code, services)
	}
	for _, raw := range services.Results {
		item := raw.(map[string]any)
		if item["service"] == "worker" && item["desired_bindings"].(map[string]any)["api"] != defaultAPIID {
			t.Fatalf("default binding did not use current worktree: %#v", item)
		}
	}
	var workerID, localAPIID, otherAPIID string
	if err := store.Update(func(reg *state.Registry) error {
		for id := range reg.Projects {
			p, err := reg.Project(id)
			if err != nil {
				return err
			}
			current := p.Worktrees[0]
			other := state.Worktree{ID: "w_other", Root: otherRoot}
			p.Worktrees = append(p.Worktrees, other)
			workerID = p.ID + "/" + current.ID + "/worker"
			localAPIID = p.ID + "/" + current.ID + "/api"
			otherAPIID = p.ID + "/" + other.ID + "/api"
			worker := p.Instances[workerID]
			worker.DesiredBindings = map[string]string{"api": otherAPIID}
			worker.EffectiveBindings = map[string]string{"api": localAPIID}
			p.Instances[workerID] = worker
			p.Instances[otherAPIID] = state.Instance{ID: otherAPIID, Service: "api", WorktreeID: other.ID, State: "stopped"}
			return reg.SetProject(id, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e, code := invoke(t, "status", "--json")
	if code != 0 || len(e.Results) != 3 {
		t.Fatalf("status: exit=%d result=%+v", code, e)
	}
	for _, raw := range e.Results {
		item := raw.(map[string]any)
		if item["instance_id"] != workerID {
			continue
		}
		if item["desired_bindings"].(map[string]any)["api"] != otherAPIID || item["effective_bindings"].(map[string]any)["api"] != localAPIID {
			t.Fatalf("status did not expose exact binding IDs: %#v", item)
		}
		return
	}
	t.Fatalf("worker result missing: %+v", e)
}

func TestDependencyBindingAndAddressDriftDoNotExposeEnvironment(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	otherRoot := filepath.Join(base, "other-worktree")
	runtime := filepath.Join(base, "state")
	t.Setenv("RDR_HOME", runtime)
	writeDependencyProject(t, root)
	writeDependencyProject(t, otherRoot)
	t.Chdir(root)
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatalf("register exit=%d", code)
	}
	store := &state.Store{Root: runtime}
	reg, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	var project state.Project
	for id := range reg.Projects {
		project, err = reg.Project(id)
		if err != nil {
			t.Fatal(err)
		}
	}
	current := project.Worktrees[0]
	other := state.Worktree{ID: "w_other", Root: otherRoot}
	project.Worktrees = append(project.Worktrees, other)
	workerID := project.ID + "/" + current.ID + "/worker"
	localAPIID := project.ID + "/" + current.ID + "/api"
	otherAPIID := project.ID + "/" + other.ID + "/api"
	worker := project.Instances[workerID]
	worker.RunID = "worker-run"
	worker.DesiredBindings = map[string]string{"api": otherAPIID}
	worker.EffectiveBindings = map[string]string{"api": localAPIID}
	localAPI := project.Instances[localAPIID]
	localAPI.RunID = "api-run"
	project.Instances[localAPIID] = localAPI
	project.Instances[workerID] = worker
	project.Instances[otherAPIID] = state.Instance{ID: otherAPIID, Service: "api", WorktreeID: other.ID, State: "stopped"}
	if err := store.Update(func(reg *state.Registry) error { return reg.SetProject(project.ID, project) }); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteManifest("worker-run", state.Manifest{RunID: "worker-run", InstanceID: workerID, EffectiveBindings: map[string]string{"api": localAPIID}, StopContract: state.StopContract{Env: map[string]string{"TOKEN": "PRIVATE_DEPENDENCY_SECRET", "RDR_DEP_API_DIRECT_URL": "http://127.0.0.1:20000", "RDR_DEP_API_PORT": "20000"}}}); err != nil {
		t.Fatal(err)
	}

	service := config.Service{DependsOn: []string{"api"}}
	for _, tc := range []struct {
		name        string
		runID       string
		desired     string
		address     map[string]string
		wantRestart bool
		wantDepErr  bool
	}{
		{name: "pending desired bind", runID: "api-run-1", desired: otherAPIID, address: map[string]string{"port": "20000", "direct_url": "http://127.0.0.1:20000", "url": "http://127.0.0.1:20000", "host": "127.0.0.1"}, wantRestart: true},
		{name: "effective dependency address changed", runID: "api-run-2", desired: localAPIID, address: map[string]string{"port": "32000", "direct_url": "http://127.0.0.1:32000", "url": "http://127.0.0.1:32000", "host": "127.0.0.1"}, wantRestart: true},
		{name: "invalid pending target remains separate", runID: "api-run-3", desired: project.ID + "/w_missing/api", address: map[string]string{"port": "20000", "direct_url": "http://127.0.0.1:20000", "url": "http://127.0.0.1:20000", "host": "127.0.0.1"}, wantRestart: true, wantDepErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker.DesiredBindings = map[string]string{"api": tc.desired}
			localAPI.RunID = tc.runID
			project.Instances[localAPIID] = localAPI
			if err := store.Update(func(reg *state.Registry) error { return reg.SetProject(project.ID, project) }); err != nil {
				t.Fatal(err)
			}
			if err := store.WriteManifest(tc.runID, state.Manifest{RunID: tc.runID, InstanceID: localAPIID, Addresses: tc.address}); err != nil {
				t.Fatal(err)
			}
			info := instanceInfo{ID: workerID, State: "running"}
			result := &envelope{Results: []any{}, Errors: []commandError{}}
			addDependencyDetails(store, project, current, &service, worker, true, &info, result)
			if !info.RestartRequired || info.RestartRequired != tc.wantRestart {
				t.Fatalf("restart_required=%t, want %t", info.RestartRequired, tc.wantRestart)
			}
			if info.DesiredBindings["api"] != tc.desired || info.EffectiveBindings["api"] != localAPIID {
				t.Fatalf("wrong binding IDs: desired=%v effective=%v", info.DesiredBindings, info.EffectiveBindings)
			}
			if (info.DependencyError != "") != tc.wantDepErr || (len(result.Errors) != 0) != tc.wantDepErr || info.State != "running" {
				t.Fatalf("dependency error overwrote state or was missing: info=%+v errors=%+v", info, result.Errors)
			}
			encoded, err := json.Marshal(info)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(encoded, []byte("PRIVATE_DEPENDENCY_SECRET")) || bytes.Contains(encoded, []byte("20000")) {
				t.Fatalf("dependency environment escaped into status: %s", encoded)
			}
		})
	}
}

func TestGraphAdapterKeepsTypedReadinessTimeout(t *testing.T) {
	err := &runner.Error{Code: "READINESS_TIMEOUT", Err: context.DeadlineExceeded}
	if code := graphFailureCode(context.Background(), err); code != "READINESS_TIMEOUT" {
		t.Fatalf("failure code = %q, want READINESS_TIMEOUT", code)
	}
	e := result("up", options{json: true})
	e.fail(graphFailureCode(context.Background(), err), err.Error())
	var out, errOut bytes.Buffer
	if code := writeResult(&out, &errOut, e, true); code != 4 {
		t.Fatalf("typed timeout exit = %d, want 4", code)
	}
	if code := graphFailureCode(context.Background(), errors.New("process failed")); code != "PROCESS_FAILED" {
		t.Fatalf("untyped failure code = %q, want PROCESS_FAILED", code)
	}
}

func TestRetryHooksFlagScopeAndDryRun(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtime := filepath.Join(base, "state")
	t.Setenv("RDR_HOME", runtime)
	writeProject(t, root)
	t.Chdir(root)
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatalf("register exit=%d", code)
	}
	before, err := os.ReadFile(filepath.Join(runtime, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{
		{"up", "worker", "--dry-run", "--retry-hooks", "--json"},
		{"down", "worker", "--dry-run", "--retry-hooks", "--json"},
		{"restart", "worker", "--dry-run", "--retry-hooks", "--json"},
	} {
		if e, code := invoke(t, command...); code != 0 || e.Plan == nil {
			t.Fatalf("%v: exit=%d result=%+v", command, code, e)
		}
	}
	for _, command := range [][]string{
		{"status", "--retry-hooks", "--json"},
		{"logs", "worker", "--retry-hooks", "--json"},
		{"bind", "worker", "api", "--to-worktree", root, "--retry-hooks", "--json"},
	} {
		if e, code := invoke(t, command...); code != 2 || len(e.Errors) != 1 || e.Errors[0].Code != "USAGE" {
			t.Fatalf("%v: exit=%d result=%+v", command, code, e)
		}
	}
	after, err := os.ReadFile(filepath.Join(runtime, "registry.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("dry-run retry changed state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runtime, "runs")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created run directory: %v", err)
	}
}

func TestHookStatusIsWhitelistedAndReadOnly(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtime := filepath.Join(base, "state")
	t.Setenv("RDR_HOME", runtime)
	writeProject(t, root)
	t.Chdir(root)
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatalf("register exit=%d", code)
	}
	store := &state.Store{Root: runtime}
	var instanceID string
	if err := store.Update(func(reg *state.Registry) error {
		for id := range reg.Projects {
			p, err := reg.Project(id)
			if err != nil {
				return err
			}
			for key, instance := range p.Instances {
				instanceID = key
				instance.Hook = &state.HookIntent{
					RunID: "saved-run", OperationID: "op-123", Name: "after_stop", Phase: "failed",
					Reason: "PRIVATE_HOOK_REASON", Child: procid.Ref{PID: 991337}, Members: []procid.Ref{{PID: 991338}},
				}
				p.Instances[key] = instance
			}
			return reg.SetProject(id, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(runtime, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	expectedLog := filepath.Join(runtime, "runs", "saved-run", "hook-after_stop.log")
	for _, command := range [][]string{
		{"status", "--json"},
		{"services", "--project", "example", "--worktree", root, "--json"},
		{"ls", "--json"},
	} {
		e, code := invoke(t, command...)
		if code != 0 || len(e.Results) != 1 {
			t.Fatalf("%v: exit=%d result=%+v", command, code, e)
		}
		encoded, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		assertPublicHook(t, encoded, expectedLog)
	}
	upResult := graphInstanceInfo(store, runner.Result{InstanceID: instanceID, StateUnknown: true}, &envelope{Results: []any{}, Errors: []commandError{}})
	encoded, err := json.Marshal(upResult)
	if err != nil {
		t.Fatal(err)
	}
	assertPublicHook(t, encoded, expectedLog)
	after, err := os.ReadFile(filepath.Join(runtime, "registry.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("read-only hook status changed registry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runtime, "runs", "saved-run")); !os.IsNotExist(err) {
		t.Fatalf("read-only hook status created run directory: %v", err)
	}
}

func assertPublicHook(t *testing.T, encoded []byte, logPath string) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(encoded, &doc); err != nil {
		t.Fatal(err)
	}
	var findHook func(any) map[string]any
	findHook = func(value any) map[string]any {
		switch item := value.(type) {
		case map[string]any:
			if hook, ok := item["hook"].(map[string]any); ok {
				return hook
			}
			for _, child := range item {
				if hook := findHook(child); hook != nil {
					return hook
				}
			}
		case []any:
			for _, child := range item {
				if hook := findHook(child); hook != nil {
					return hook
				}
			}
		}
		return nil
	}
	hook := findHook(doc)
	if hook == nil || len(hook) != 5 || hook["run_id"] != "saved-run" || hook["operation_id"] != "op-123" || hook["name"] != "after_stop" || hook["phase"] != "failed" || hook["log_path"] != logPath {
		t.Fatalf("invalid public hook status: %s", encoded)
	}
	for _, private := range []string{"child", "members", "PRIVATE_HOOK_REASON", "991337", "991338"} {
		if bytes.Contains(encoded, []byte(private)) {
			t.Fatalf("hook internals leaked (%s): %s", private, encoded)
		}
	}
}
