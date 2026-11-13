package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
	if _, code = invoke(t, "down", "--all", "--json"); code != 2 {
		t.Fatalf("broad down exit=%d", code)
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
