package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
