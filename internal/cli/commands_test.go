// Tests cli commands behavior.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/proxy"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
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

func TestVersionWithoutRuntime(t *testing.T) {
	runtime := filepath.Join(t.TempDir(), "absent")
	t.Setenv("RDR_HOME", runtime)
	e, code := invoke(t, "version", "--json")
	if code != 0 || len(e.Results) != 1 {
		t.Fatalf("version: %d %+v", code, e)
	}
	info := e.Results[0].(map[string]any)
	if info["name"] != "rundevrun" || info["api_version"] != float64(1) || info["version"] != Version {
		t.Fatalf("version contract: %+v", info)
	}
	if _, err := os.Stat(runtime); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("version created runtime: %v", err)
	}
}

func cliRunIDs(t *testing.T, project string) map[string]string {
	t.Helper()
	e, code := invoke(t, "status", "--project", project, "--json")
	if code != 0 {
		t.Fatalf("status: %d %+v", code, e)
	}
	ids := map[string]string{}
	for _, item := range e.Results {
		row := item.(map[string]any)
		id, _ := row["instance_id"].(string)
		runID, _ := row["run_id"].(string)
		ids[id] = runID
	}
	return ids
}

func invokeBinary(t *testing.T, binary string, args ...string) (envelope, int) {
	t.Helper()
	cmd := exec.Command(binary, append(args, "--json")...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatalf("run %s: %v", binary, err)
		}
	}
	var result envelope
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON from %s: stdout=%q stderr=%q: %v", binary, stdout.String(), stderr.String(), err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr from %s: %s", binary, stderr.String())
	}
	return result, code
}

func writeProject(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(`version = 1
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
	for _, args := range [][]string{{"services", "--project", "example", "--json"}, {"status", "--project", "example", "--json"}, {"status", "--json"}} {
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
	if err := os.WriteFile("rundevrun.toml", []byte("version = 2\n"), 0600); err != nil {
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

func TestProxyStatusReadOnlyAndGrammar(t *testing.T) {
	base := t.TempDir()
	runtime := filepath.Join(base, "runtime")
	cfgDir := filepath.Join(base, "config")
	if err := os.MkdirAll(cfgDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("invalid = ["), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RDR_HOME", runtime)
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	e, code := invoke(t, "proxy", "status", "--json")
	if code != 0 || e.Proxy == nil || e.Proxy.State != "stopped" {
		t.Fatalf("proxy status: %d %+v", code, e)
	}
	if _, err := os.Stat(runtime); !os.IsNotExist(err) {
		t.Fatalf("readonly status created runtime: %v", err)
	}
	if _, code = invoke(t, "proxy", "status", "--project", "x", "--json"); code != 2 {
		t.Fatalf("proxy selector accepted: %d", code)
	}
	for _, args := range [][]string{{"proxy", "stop", "--no-proxy", "--json"}, {"logs", "worker", "--no-proxy", "--json"}, {"status", "--no-proxy", "--json"}} {
		if _, code := invoke(t, args...); code != 2 {
			t.Fatalf("invalid no-proxy scope accepted %v: %d", args, code)
		}
	}
	if e, code := invoke(t, "proxy", "reload", "--dry-run", "--json"); code != 2 || len(e.Errors) != 1 || e.Errors[0].Code != "NOT_FOUND" {
		t.Fatalf("reload dry run should report stopped daemon before reading config: %d %+v", code, e)
	}
	encoded, _ := json.Marshal(e)
	if bytes.Contains(encoded, []byte("identity")) || bytes.Contains(encoded, []byte("boot")) {
		t.Fatalf("proxy identity leaked: %s", encoded)
	}
}

func TestDownAndLogsDoNotParseMachineRoutingConfig(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtime := filepath.Join(base, "state")
	cfg := filepath.Join(base, "config")
	t.Setenv("RDR_HOME", runtime)
	t.Setenv("XDG_CONFIG_HOME", cfg)
	writeProject(t, root)
	t.Chdir(root)
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatalf("register: %d", code)
	}
	if err := os.MkdirAll(cfg, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "config.toml"), []byte("bad = ["), 0600); err != nil {
		t.Fatal(err)
	}
	if e, code := invoke(t, "down", "worker", "--json"); code != 0 || len(e.ProxyErrors) > 0 {
		t.Fatalf("down parsed bad routing config: %d %+v", code, e)
	}
	if e, code := invoke(t, "logs", "worker", "--json"); code == 0 || len(e.Errors) == 0 || e.Errors[0].Code == "CONFIG_INVALID" {
		t.Fatalf("logs parsed bad routing config: %d %+v", code, e)
	}
}

func TestInvalidMachineRoutingPreflightsStartCommandsWithoutMutation(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtime := filepath.Join(base, "state")
	cfgDir := filepath.Join(base, "config")
	t.Setenv("RDR_HOME", runtime)
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	writeProject(t, root)
	t.Chdir(root)
	registered, code := invoke(t, "register", ".", "--json")
	if code != 0 {
		t.Fatalf("register: %d %+v", code, registered)
	}
	project := registered.Scope.Project
	if err := os.MkdirAll(filepath.Join(cfgDir, "rundevrun"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "rundevrun", "config.toml"), []byte("broken = ["), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(runtime, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"up", "worker"}, {"restart", "worker"}, {"switch", "worker", "--project", project, "--worktree", root, "--to-worktree", root}, {"up", "worker", "--dry-run"}, {"switch", "worker", "--project", project, "--worktree", root, "--to-worktree", root, "--dry-run"}} {
		e, code := invoke(t, append(args, "--json")...)
		if code != 2 || len(e.Errors) == 0 || e.Errors[0].Code != "CONFIG_INVALID" {
			t.Fatalf("%v did not preflight bad routing config: %d %+v", args, code, e)
		}
		after, err := os.ReadFile(filepath.Join(runtime, "registry.json"))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("%v mutated registry before config validation: %v", args, err)
		}
		if _, err := os.Stat(filepath.Join(runtime, "runs")); !os.IsNotExist(err) {
			t.Fatalf("%v created run state before config validation: %v", args, err)
		}
	}
}

func TestReregisterPreservesWorktreeAliasAndRouteBookURL(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtime := filepath.Join(base, "state")
	t.Setenv("RDR_HOME", runtime)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	writeProject(t, root)
	t.Chdir(root)
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatalf("register: %d", code)
	}
	store := &state.Store{Root: runtime}
	var project state.Project
	if err := store.Update(func(reg *state.Registry) error {
		for id := range reg.Projects {
			p, err := reg.Project(id)
			if err != nil {
				return err
			}
			project = p
			project.Worktrees[0].Alias = "remembered"
			inst := project.Instances[project.ID+"/"+project.Worktrees[0].ID+"/worker"]
			inst.RouteURL = "https://worker-remembered.example.test"
			project.Instances[inst.ID] = inst
			return reg.SetProject(id, project)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatalf("reregister: %d", code)
	}
	reg, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reg.Project(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Worktrees[0].Alias != "remembered" {
		t.Fatalf("alias lost on refresh: %+v", saved.Worktrees[0])
	}
	target := saved.Instances[project.ID+"/"+saved.Worktrees[0].ID+"/worker"]
	if target.RouteURL != "https://worker-remembered.example.test" {
		t.Fatalf("route book changed on disabled registration: %+v", target)
	}
	e, code := invoke(t, "status", "--project", project.ID, "--json")
	if code != 0 {
		t.Fatalf("status: %d %+v", code, e)
	}
	item := e.Results[0].(map[string]any)
	if item["url"] != target.RouteURL {
		t.Fatalf("stopped route URL missing: %+v", item)
	}
	services, code := invoke(t, "services", "--project", project.ID, "--worktree", root, "--json")
	if code != 0 || services.Results[0].(map[string]any)["url"] != target.RouteURL {
		t.Fatalf("service route URL: %d %+v", code, services)
	}
	if err := os.MkdirAll(filepath.Join(base, "config", "rundevrun"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "config", "rundevrun", "config.toml"), []byte("broken = ["), 0600); err != nil {
		t.Fatal(err)
	}
	if e, code := invoke(t, "register", ".", "--json"); code != 5 || e.Status != "partial" || len(e.RoutingErrors) != 1 {
		t.Fatalf("invalid routing config should retain registration with partial status: %d %+v", code, e)
	}
	reg, err = store.Read()
	if err != nil {
		t.Fatal(err)
	}
	saved, err = reg.Project(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	target = saved.Instances[target.ID]
	if target.RouteURL != "https://worker-remembered.example.test" {
		t.Fatalf("failed route assignment changed old route: %+v", target)
	}
}

func TestRouteURLChangeRequiresRestartAndDirectURLStaysManifestValue(t *testing.T) {
	store := &state.Store{Root: t.TempDir()}
	instance := state.Instance{ID: "p/w/api", RunID: "route-run", RouteURL: "https://new.example.test"}
	manifest := state.Manifest{RunID: "route-run", InstanceID: instance.ID, Launch: state.LaunchContract{Protocol: "http"}, Addresses: map[string]string{"url": "https://old.example.test", "direct_url": "http://127.0.0.1:31313"}}
	if err := store.WriteManifest(instance.RunID, manifest); err != nil {
		t.Fatal(err)
	}
	info := instanceInfo{State: "running"}
	addRunDetails(store, instance, &info)
	applyRouteView(store, instance, &info)
	if info.URL != instance.RouteURL || info.DirectURL != manifest.Addresses["direct_url"] || !info.RouteChanged || !info.RestartRequired {
		t.Fatalf("route overlay/drift wrong: %+v", info)
	}
}

func TestDisabledRouteUsesDirectURLAndFlagsActiveRouteDrift(t *testing.T) {
	store := &state.Store{Root: t.TempDir()}
	instance := state.Instance{ID: "p/w/api", RunID: "route-run"}
	manifest := state.Manifest{RunID: "route-run", InstanceID: instance.ID, Launch: state.LaunchContract{Protocol: "http"}, Addresses: map[string]string{"url": "https://stale.example.test", "direct_url": "http://127.0.0.1:31313"}}
	if err := store.WriteManifest(instance.RunID, manifest); err != nil {
		t.Fatal(err)
	}
	info := instanceInfo{State: "running"}
	addRunDetails(store, instance, &info)
	applyRouteView(store, instance, &info)
	if info.URL != manifest.Addresses["direct_url"] || !info.RouteChanged || !info.RestartRequired {
		t.Fatalf("disabled routing did not choose direct URL/drift: %+v", info)
	}
}

func TestGraphNeedsProxySkipsProcessOnlyAndFollowsHTTPBinding(t *testing.T) {
	makeStore := func(t *testing.T, withAPI bool) *state.Store {
		t.Helper()
		store, err := state.New(filepath.Join(t.TempDir(), "state"))
		if err != nil {
			t.Fatal(err)
		}
		p := state.Project{ID: "p", Name: "p", Root: "/p", Worktrees: []state.Worktree{{ID: "w", Root: "/p"}}, Instances: map[string]state.Instance{"p/w/worker": {ID: "p/w/worker", Service: "worker", WorktreeID: "w", State: "running", RunID: "worker-run"}}}
		if withAPI {
			p.Instances["p/w/api"] = state.Instance{ID: "p/w/api", Service: "api", WorktreeID: "w", State: "ready", RunID: "api-run", RouteURL: "https://api.example.test"}
		}
		if err := store.Update(func(reg *state.Registry) error { return reg.SetProject("p", p) }); err != nil {
			t.Fatal(err)
		}
		worker := state.Manifest{RunID: "worker-run", InstanceID: "p/w/worker", Launch: state.LaunchContract{Protocol: "process"}}
		if withAPI {
			worker.EffectiveBindings = map[string]string{"api": "p/w/api"}
		}
		if err := store.WriteManifest("worker-run", worker); err != nil {
			t.Fatal(err)
		}
		if withAPI {
			if err := store.WriteManifest("api-run", state.Manifest{RunID: "api-run", InstanceID: "p/w/api", Launch: state.LaunchContract{Protocol: "http"}}); err != nil {
				t.Fatal(err)
			}
		}
		return store
	}
	result := []runner.Result{{InstanceID: "p/w/worker", RunID: "worker-run", State: "running", Status: "completed"}}
	needs, err := graphNeedsProxy(makeStore(t, false), result)
	if err != nil || needs {
		t.Fatalf("process-only graph needs proxy: %t %v", needs, err)
	}
	needs, err = graphNeedsProxy(makeStore(t, true), result)
	if err != nil || !needs {
		t.Fatalf("routed effective dependency did not need proxy: %t %v", needs, err)
	}
}

func TestProxyReloadDryRunRequiresMatchingActiveListeners(t *testing.T) {
	cfg := routing.Config{Routing: routing.Settings{HTTPListen: "127.0.0.1:8080", HTTPSListen: "127.0.0.1:8443"}}
	err := proxyReloadCompatibility(proxy.Info{State: "ready", HTTPListen: "127.0.0.1:8081", HTTPSListen: "127.0.0.1:8443", TLSMode: "provided"}, cfg)
	var pe *proxy.Error
	if !errors.As(err, &pe) || pe.Code != "CONFIG_INVALID" {
		t.Fatalf("listener mismatch should require restart: %v", err)
	}
}

func TestAliasProxyStatusFailureKeepsMetadataResult(t *testing.T) {
	store, err := state.New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Root, "proxy.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	e := result("alias", options{})
	e.Results = append(e.Results, map[string]any{"instance_id": "p/w/api", "new_url": "https://api.example.test"})
	refreshProxyAfterAlias(context.Background(), store, &e)
	if e.Status != "partial" || len(e.ProxyErrors) != 1 || e.Results[0].(map[string]any)["new_url"] != "https://api.example.test" {
		t.Fatalf("proxy status failure hid successful alias metadata: %+v", e)
	}
}

func TestProxyAndRoutingErrorsKeepStableCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{{&proxy.Error{Code: "BUSY", Message: "busy"}, "BUSY"}, {&routing.Error{Code: "TLS_INVALID", Err: errors.New("bad cert")}, "TLS_INVALID"}, {state.ErrOwnershipUnknown, "OWNERSHIP_UNKNOWN"}} {
		e := result("proxy status", options{})
		failError(&e, tc.err)
		if e.Errors[0].Code != tc.want {
			t.Fatalf("%T code=%s want %s", tc.err, e.Errors[0].Code, tc.want)
		}
	}
}

func TestProxyFailureIsSeparateFromSuccessfulAppState(t *testing.T) {
	e := result("up", options{})
	e.Results = append(e.Results, map[string]any{"instance_id": "p/w/api", "state": "ready", "status": "completed"})
	appendProxyError(context.Background(), &e, &proxy.Error{Code: "BUSY", Message: "proxy unavailable"})
	if e.Status != "partial" || len(e.ProxyErrors) != 1 || len(e.Errors) != 0 {
		t.Fatalf("proxy error envelope: %+v", e)
	}
	item := e.Results[0].(map[string]any)
	if item["state"] != "ready" || item["status"] != "completed" {
		t.Fatalf("proxy failure replaced app state: %+v", item)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e = result("up", options{})
	appendProxyError(ctx, &e, errors.New("wrapped proxy error"))
	if len(e.Errors) == 0 || e.Errors[0].Code != "CANCELLED" {
		t.Fatalf("cancel was not prioritized: %+v", e)
	}
}

func TestOpenPrintsOnlyRegisteredURLWithoutStartingServices(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtimeRootPath := filepath.Join(base, "runtime")
	t.Setenv("RDR_HOME", runtimeRootPath)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	writeProject(t, root)
	t.Chdir(root)
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatalf("register: %d", code)
	}
	store := &state.Store{Root: runtimeRootPath}
	var id string
	if err := store.Update(func(reg *state.Registry) error {
		for projectID := range reg.Projects {
			p, err := reg.Project(projectID)
			if err != nil {
				return err
			}
			for key, instance := range p.Instances {
				id = key
				instance.RouteURL = "https://worker-main.example.test"
				p.Instances[key] = instance
			}
			return reg.SetProject(projectID, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(runtimeRootPath, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	e, code := invoke(t, "open", "worker", "--project", "example", "--worktree", root, "--json")
	if code != 0 || len(e.Results) != 1 || e.Results[0].(map[string]any)["url"] != "https://worker-main.example.test" {
		t.Fatalf("open: %d %+v", code, e)
	}
	encoded, _ := json.Marshal(e)
	if bytes.Contains(encoded, []byte(id)) || bytes.Contains(encoded, []byte("state")) {
		t.Fatalf("open JSON included data beyond URL: %s", encoded)
	}
	after, err := os.ReadFile(filepath.Join(runtimeRootPath, "registry.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("open mutated route book: %v", err)
	}
	for _, name := range []string{"runs", "proxy.json"} {
		if _, err := os.Stat(filepath.Join(runtimeRootPath, name)); !os.IsNotExist(err) {
			t.Fatalf("open created %s: %v", name, err)
		}
	}
}

func TestOpenFallsBackOnlyToVerifiedActiveHTTPManifest(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtimeRootPath := filepath.Join(base, "runtime")
	t.Setenv("RDR_HOME", runtimeRootPath)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	writeProject(t, root)
	t.Chdir(root)
	if _, code := invoke(t, "register", ".", "--json"); code != 0 {
		t.Fatalf("register: %d", code)
	}
	store := &state.Store{Root: runtimeRootPath}
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
	instanceID := project.ID + "/" + project.Worktrees[0].ID + "/worker"
	instance := project.Instances[instanceID]
	instance.RunID = "http-run"
	instance.State = "ready"
	project.Instances[instanceID] = instance
	if err := store.Update(func(reg *state.Registry) error { return reg.SetProject(project.ID, project) }); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteManifest("http-run", state.Manifest{RunID: "http-run", InstanceID: instanceID, Launch: state.LaunchContract{Protocol: "http"}, Addresses: map[string]string{"direct_url": "http://127.0.0.1:32100", "url": "https://stale.example.test"}}); err != nil {
		t.Fatal(err)
	}
	if e, code := invoke(t, "open", "worker", "--project", project.ID, "--worktree", root, "--json"); code != 2 || e.Errors[0].Code != "NOT_FOUND" {
		t.Fatalf("unverified direct URL was exposed: %d %+v", code, e)
	}
	if err := store.WriteManifest("worker-process-run", state.Manifest{RunID: "worker-process-run", InstanceID: instanceID, Launch: state.LaunchContract{Protocol: "process"}, Addresses: map[string]string{"direct_url": "http://127.0.0.1:32100"}}); err != nil {
		t.Fatal(err)
	}
	instance.RunID = "worker-process-run"
	if err := store.Update(func(reg *state.Registry) error {
		p, err := reg.Project(project.ID)
		if err != nil {
			return err
		}
		p.Instances[instanceID] = instance
		return reg.SetProject(project.ID, p)
	}); err != nil {
		t.Fatal(err)
	}
	if e, code := invoke(t, "open", "worker", "--project", project.ID, "--worktree", root, "--json"); code != 2 || e.Errors[0].Code != "NOT_FOUND" {
		t.Fatalf("process service opened by fallback: %d %+v", code, e)
	}
}

func TestDoctorReportsChecksReadOnlyAndReturnsFailureExit(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtimeRootPath := filepath.Join(base, "runtime")
	t.Setenv("RDR_HOME", runtimeRootPath)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	writeProject(t, root)
	t.Chdir(root)
	registered, code := invoke(t, "register", ".", "--json")
	if code != 0 {
		t.Fatalf("register: %d %+v", code, registered)
	}
	project := registered.Scope.Project
	configPath := filepath.Join(base, "config", "rundevrun", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("not = [valid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(runtimeRootPath, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	e, code := invoke(t, "doctor", "--project", project, "--worktree", root, "--json")
	if code != 4 || e.Status != "error" || len(e.Errors) != 1 || e.Errors[0].Code != "DIAGNOSTICS_FAILED" || len(e.Results) != 1 {
		t.Fatalf("doctor failure envelope: %d %+v", code, e)
	}
	report := e.Results[0].(map[string]any)["report"].(map[string]any)
	if report["configuration"].(map[string]any)["status"] != "fail" || len(report["routes"].([]any)) != 1 {
		t.Fatalf("doctor omitted individual checks: %#v", report)
	}
	after, err := os.ReadFile(filepath.Join(runtimeRootPath, "registry.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("doctor wrote registry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeRootPath, "proxy.json")); !os.IsNotExist(err) {
		t.Fatalf("doctor wrote proxy state: %v", err)
	}
}

func TestPublicationGrammarAndDryRunsDoNotCreatePublicationState(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtimeRootPath := filepath.Join(base, "state")
	t.Setenv("RDR_HOME", runtimeRootPath)
	writeProject(t, root)
	t.Chdir(root)
	registered, code := invoke(t, "register", ".", "--json")
	if code != 0 {
		t.Fatalf("register: %d %+v", code, registered)
	}
	project := registered.Scope.Project
	flags := []struct {
		args []string
		code int
	}{
		{[]string{"share", "worker", "--project", project, "--worktree", root, "--mode", "preview", "--json"}, 2},
		{[]string{"unshare", "worker", "--project", project, "--worktree", root, "--recreate", "--json"}, 2},
	}
	for _, tc := range flags {
		if _, code := invoke(t, tc.args...); tc.code >= 0 && code != tc.code || tc.code < 0 && code == 0 {
			t.Fatalf("%v exit=%d want %d", tc.args, code, tc.code)
		}
	}
	if e, code := invoke(t, "share", "worker", "--project", project, "--worktree", root, "--mode", "snapshot", "--dry-run", "--json"); code == 0 || len(e.Errors) != 1 || e.Errors[0].Code == "USAGE" {
		t.Fatalf("snapshot mode should reach the snapshot planner: %d %+v", code, e)
	}
	if _, err := os.Stat(filepath.Join(runtimeRootPath, "publications.json")); !os.IsNotExist(err) {
		t.Fatalf("rejected/dry-run share wrote publication registry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeRootPath, "publications")); !os.IsNotExist(err) {
		t.Fatalf("rejected/dry-run share created publication directory: %v", err)
	}
	e, code := invoke(t, "unshare", "worker", "--project", project, "--worktree", root, "--dry-run", "--json")
	if code != 0 || e.Plan == nil {
		t.Fatalf("unshare dry-run: %d %+v", code, e)
	}
	if _, err := os.Stat(filepath.Join(runtimeRootPath, "publications.json")); !os.IsNotExist(err) {
		t.Fatalf("unshare dry-run wrote publication registry: %v", err)
	}
}

func TestShareDryRunUsesConfiguredSnapshotAndExplicitLiveOverride(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	base, err := os.MkdirTemp("", "rdr-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	modFile, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("locate module: %v", err)
	}
	repoRoot := filepath.Dir(strings.TrimSpace(string(modFile)))
	root := filepath.Join(base, "project")
	runtimeRootPath := filepath.Join(base, "state")
	t.Setenv("RDR_HOME", runtimeRootPath)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	proxyListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyPort := proxyListener.Addr().(*net.TCPAddr).Port
	proxyListener.Close()
	configRoot := filepath.Join(base, "config", "rundevrun")
	if err := os.MkdirAll(configRoot, 0700); err != nil {
		t.Fatal(err)
	}
	settings := fmt.Sprintf(`{"version":1,"local":{"listen":"127.0.0.1:%d"},"tailnet":{}}`, proxyPort)
	if err := os.WriteFile(filepath.Join(configRoot, "networking.json"), []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server.py"), []byte(`import os
from http.server import BaseHTTPRequestHandler, HTTPServer
class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ready")
HTTPServer(("127.0.0.1", int(os.environ["RDR_PORT"])), Handler).serve_forever()
`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(fmt.Sprintf(`version = 1
[project]
name = "example"
[services.web]
command = "python3 server.py"
protocol = "http"
port = %d
health = { tcp = true }
[services.web.share]
mode = "snapshot"
build = "touch snapshot-built"
dir = "dist"
`, port)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	registered, code := invoke(t, "register", ".", "--json")
	if code != 0 {
		t.Fatalf("register: %d %+v", code, registered)
	}
	project := registered.Scope.Project
	binary := filepath.Join(base, "rundevrun")
	build := exec.Command("go", "build", "-o", binary, "./cmd/rundevrun")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build rundevrun binary: %v: %s", err, output)
	}
	t.Cleanup(func() {
		_, _ = invokeBinary(t, binary, "down", "web", "--project", project, "--worktree", root)
		_, _ = invokeBinary(t, binary, "proxy", "stop")
	})
	if e, code := invokeBinary(t, binary, "up", "web", "--project", project, "--worktree", root); code != 0 {
		logs, logCode := invokeBinary(t, binary, "logs", "web", "--project", project, "--worktree", root, "--tail", "30")
		t.Fatalf("up: %d %+v; logs: %d %+v", code, e, logCode, logs)
	}
	before := cliRunIDs(t, project)
	for _, tc := range []struct {
		flags []string
		mode  string
	}{
		{nil, "snapshot"},
		{[]string{"--mode", "snapshot"}, "snapshot"},
		{[]string{"--mode", "live"}, "live"},
	} {
		args := []string{"share", "web", "--project", project, "--worktree", root, "--dry-run", "--json"}
		args = append(args, tc.flags...)
		e, code := invoke(t, args...)
		if code != 0 || e.Plan == nil {
			t.Fatalf("dry-run %q: %d %+v", tc.flags, code, e)
		}
		plan := e.Plan.(map[string]any)["plan"].(map[string]any)
		if plan["mode"] != tc.mode {
			t.Fatalf("dry-run %q selected mode %v, want %s", tc.flags, plan["mode"], tc.mode)
		}
		if tc.mode == "snapshot" && plan["snapshot"] == nil || tc.mode == "live" && plan["live"] == nil {
			t.Fatalf("dry-run %q omitted selected plan: %#v", tc.flags, plan)
		}
	}
	if e, code := invoke(t, "share", "web", "--project", project, "--worktree", root, "--mode", "invalid", "--dry-run", "--json"); code != 2 || len(e.Errors) != 1 || e.Errors[0].Code != "USAGE" {
		t.Fatalf("invalid mode: %d %+v", code, e)
	}
	if !reflect.DeepEqual(before, cliRunIDs(t, project)) {
		t.Fatal("share dry-run changed application run IDs")
	}
	if _, err := os.Stat(filepath.Join(root, "snapshot-built")); !os.IsNotExist(err) {
		t.Fatalf("snapshot dry-run executed build: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeRootPath, "publications.json")); !os.IsNotExist(err) {
		t.Fatalf("share dry-run wrote publication state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeRootPath, "publications")); !os.IsNotExist(err) {
		t.Fatalf("share dry-run created publication directory: %v", err)
	}
}

func TestPublicationStatusIsWhitelistedAndReadOnly(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtimeRootPath := filepath.Join(base, "state")
	t.Setenv("RDR_HOME", runtimeRootPath)
	writeProject(t, root)
	t.Chdir(root)
	registered, code := invoke(t, "register", ".", "--json")
	if code != 0 {
		t.Fatalf("register: %d %+v", code, registered)
	}
	otherRoot := filepath.Join(base, "other-project")
	writeProject(t, otherRoot)
	t.Chdir(otherRoot)
	otherRegistered, code := invoke(t, "register", ".", "--json")
	if code != 0 {
		t.Fatalf("register other project: %d %+v", code, otherRegistered)
	}
	t.Chdir(root)
	reg, err := (&state.Store{Root: runtimeRootPath}).Read()
	if err != nil {
		t.Fatal(err)
	}
	project, err := reg.Project(registered.Scope.Project)
	if err != nil || len(project.Worktrees) != 1 {
		t.Fatalf("registered worktree: %+v %v", project, err)
	}
	otherProject, err := reg.Project(otherRegistered.Scope.Project)
	if err != nil || len(otherProject.Worktrees) != 1 {
		t.Fatalf("other registered worktree: %+v %v", otherProject, err)
	}
	store := &state.Store{Root: runtimeRootPath}
	owner, err := procid.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	publicationRegistry := state.PublicationRegistry{Version: 1, Publications: map[string]state.Publication{
		"share-safe": {
			ID: "share-safe", ProjectID: project.ID, RootID: project.ID + "/" + project.Worktrees[0].ID + "/worker", RootRunID: "run-safe",
			Mode: "live", Phase: "preparing", Cloudflared: "/usr/bin/cloudflared", Owner: owner,
			Salt: bytes.Repeat([]byte("S"), 16), Verifier: bytes.Repeat([]byte("V"), 32), SigningKey: bytes.Repeat([]byte("K"), 32),
		},
		"share-other": {
			ID: "share-other", ProjectID: otherProject.ID, RootID: otherProject.ID + "/" + otherProject.Worktrees[0].ID + "/worker", RootRunID: "other-run",
			Mode: "live", Phase: "preparing", Cloudflared: "/usr/bin/cloudflared", Owner: owner,
			Salt: bytes.Repeat([]byte("T"), 16), Verifier: bytes.Repeat([]byte("W"), 32), SigningKey: bytes.Repeat([]byte("L"), 32),
		},
	}}
	lock, err := store.LockPublicationRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WritePublications(publicationRegistry); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(runtimeRootPath, "publications.json"))
	if err != nil {
		t.Fatal(err)
	}
	e, code := invoke(t, "status", "--json")
	if code != 0 || len(e.Publications) != 2 || e.Publications[0].State != "starting" || e.Publications[1].State != "starting" {
		t.Fatalf("status publications: %d %+v", code, e)
	}
	encoded, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SigningKey", "signing_key", "Verifier", "verifier", "Salt", "salt", "Owner", "owner", "Cloudflared", "cloudflared", "secret-password"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("status exposed publication secret %q: %s", secret, encoded)
		}
	}
	scoped, code := invoke(t, "status", "--project", project.ID, "--worktree", root, "--json")
	if code != 0 || len(scoped.Publications) != 1 || scoped.Publications[0].ID != "share-safe" {
		t.Fatalf("publication scope filter: %d %+v", code, scoped)
	}
	after, err := os.ReadFile(filepath.Join(runtimeRootPath, "publications.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("status wrote publication registry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeRootPath, "publications", "share-safe", "runtime.json")); !os.IsNotExist(err) {
		t.Fatalf("status created publication runtime: %v", err)
	}
}

func TestPublicationStatusFailureKeepsAppStateVisibleAndCancellationWins(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	runtimeRootPath := filepath.Join(base, "state")
	t.Setenv("RDR_HOME", runtimeRootPath)
	writeProject(t, root)
	t.Chdir(root)
	registered, code := invoke(t, "register", ".", "--json")
	if code != 0 {
		t.Fatalf("register: %d %+v", code, registered)
	}
	pubPath := filepath.Join(runtimeRootPath, "publications.json")
	if err := os.WriteFile(pubPath, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	e, code := invoke(t, "status", "--project", registered.Scope.Project, "--json")
	if code != 5 || e.Status != "partial" || len(e.Results) != 1 || len(e.PublicationErrors) != 1 {
		t.Fatalf("publication read error hid app state: %d %+v", code, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	code = Run(ctx, []string{"share", "worker", "--project", registered.Scope.Project, "--worktree", root, "--json"}, &out, &errOut)
	var cancelled envelope
	if err := json.Unmarshal(out.Bytes(), &cancelled); err != nil || code != 130 || len(cancelled.Errors) == 0 || cancelled.Errors[0].Code != "CANCELLED" {
		t.Fatalf("cancelled share: code=%d err=%v envelope=%+v stderr=%s", code, err, cancelled, errOut.String())
	}
}

func TestLifecyclePlanAndSingleServiceGrammar(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	t.Setenv("RDR_HOME", filepath.Join(base, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	writeProject(t, root)
	t.Chdir(root)
	if e, code := invoke(t, "register", ".", "--json"); code != 0 || e.Status != "ok" {
		t.Fatalf("register: %d %+v", code, e)
	}
	e, code := invoke(t, "up", "worker", "--dry-run", "--json")
	if code != 0 || e.Plan == nil || len(e.Results) != 0 {
		t.Fatalf("dry run: %d %+v", code, e)
	}
	if e, code = invoke(t, "up", "worker", "--dry-run", "--no-proxy", "--json"); code != 0 || e.Plan == nil {
		t.Fatalf("no-proxy dry run: %d %+v", code, e)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"up", "worker", "--dry-run"}, "Prepare → Build → Start → Wait for readiness"},
		{[]string{"down", "worker", "--dry-run"}, "Keep stopped"},
		{[]string{"restart", "worker", "--dry-run"}, "Plan for rundevrun restart"},
		{[]string{"config", "check"}, "Configuration valid"},
		{[]string{"projects"}, "PROJECT"},
		{[]string{"worktrees"}, "WORKTREE"},
		{[]string{"network", "check"}, "Tailscale Serve"},
		{[]string{"network", "plan"}, "Network plan"},
	} {
		var out, errOut bytes.Buffer
		if code := Run(context.Background(), tc.args, &out, &errOut); code != 0 || !strings.Contains(out.String(), tc.want) {
			t.Fatalf("rundevrun %v: exit=%d stdout=%q stderr=%q, want %q", tc.args, code, out.String(), errOut.String(), tc.want)
		}
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
	if err := os.Remove(filepath.Join(root, "rundevrun.toml")); err != nil {
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
	if err := os.WriteFile("rundevrun.toml", []byte(`version=1
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
	if err := os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte("invalid = ["), 0600); err != nil {
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
	if err := os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(`version = 1
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
	if err := store.WriteManifest("worker-run", state.Manifest{RunID: "worker-run", InstanceID: workerID, EffectiveBindings: map[string]string{"api": localAPIID}, StopContract: state.StopContract{Env: map[string]string{"TOKEN": "PRIVATE_DEPENDENCY_SECRET", "RDR_DEP_API_DIRECT_URL": "http://127.0.0.1:20000", "RDR_DEP_API_PORT": "20000", "RDR_DEP_API_URL": "http://127.0.0.1:20000"}}}); err != nil {
		t.Fatal(err)
	}

	service := config.Service{DependsOn: []string{"api"}}
	for _, tc := range []struct {
		name        string
		runID       string
		desired     string
		address     map[string]string
		routeURL    string
		wantRestart bool
		wantDepErr  bool
	}{
		{name: "pending desired bind", runID: "api-run-1", desired: otherAPIID, address: map[string]string{"port": "20000", "direct_url": "http://127.0.0.1:20000", "url": "http://127.0.0.1:20000", "host": "127.0.0.1"}, wantRestart: true},
		{name: "effective dependency address changed", runID: "api-run-2", desired: localAPIID, address: map[string]string{"port": "32000", "direct_url": "http://127.0.0.1:32000", "url": "http://127.0.0.1:32000", "host": "127.0.0.1"}, wantRestart: true},
		{name: "effective routed URL changed", runID: "api-run-route", desired: localAPIID, routeURL: "https://api-new.example.test", address: map[string]string{"port": "20000", "direct_url": "http://127.0.0.1:20000", "url": "http://127.0.0.1:20000", "host": "127.0.0.1"}, wantRestart: true},
		{name: "invalid pending target remains separate", runID: "api-run-3", desired: project.ID + "/w_missing/api", address: map[string]string{"port": "20000", "direct_url": "http://127.0.0.1:20000", "url": "http://127.0.0.1:20000", "host": "127.0.0.1"}, wantRestart: true, wantDepErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worker.DesiredBindings = map[string]string{"api": tc.desired}
			localAPI.RunID = tc.runID
			localAPI.RouteURL = tc.routeURL
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

func TestSwitchFileStrictGrammar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "switch.json")
	valid := `{"version":1,"mappings":[{"from":{"project":"p","worktree":"a","service":"api"},"to":{"project":"p","worktree":"b","service":"api"}}]}`
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	if f, err := readSwitchFile(path); err != nil || len(f.Mappings) != 1 {
		t.Fatalf("valid file: %+v %v", f, err)
	}
	for _, input := range []string{valid + ` {}`, `{"version":1,"extra":true,"mappings":[]}`, `{"version":2,"mappings":[]}`, `{"version":1,"mappings":[{"from":{"project":"p"}}]}`} {
		if err := os.WriteFile(path, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readSwitchFile(path); err == nil {
			t.Fatalf("accepted invalid file: %s", input)
		}
	}
}

func TestSwitchJournalStatusIsReadOnlyAndWhitelisted(t *testing.T) {
	runtime := filepath.Join(t.TempDir(), "state")
	t.Setenv("RDR_HOME", runtime)
	store := &state.Store{Root: runtime}
	op := state.Operation{ID: "op-test", Kind: "switch", Status: "running", Plan: json.RawMessage(`{"mappings":[{"from":"p/w/api","to":"p/other/api"}]}`), Runs: map[string]string{"p/w/api": "private-run-secret"}, Steps: []state.OperationStep{{ID: "start:p/other/api", Status: "started"}}}
	if err := store.WriteOperation(op); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(runtime, "operations", "op-test.json"))
	if err != nil {
		t.Fatal(err)
	}
	e, code := invoke(t, "status", "--json")
	if code != 4 || len(e.Operations) != 1 || len(e.Errors) != 1 || e.Errors[0].Code != "OWNERSHIP_UNKNOWN" {
		t.Fatalf("status: %d %+v", code, e)
	}
	encoded, _ := json.Marshal(e)
	if bytes.Contains(encoded, []byte("private-run-secret")) || bytes.Contains(encoded, []byte(`"runs"`)) || bytes.Contains(encoded, []byte(`"plan"`)) {
		t.Fatalf("journal leaked private fields: %s", encoded)
	}
	if e.Operations[0].(map[string]any)["operation_state_unknown"] != true || e.Operations[0].(map[string]any)["operation_interrupted"] != nil {
		t.Fatalf("unknown owner must stay distinct from interrupted: %+v", e.Operations[0])
	}
	after, err := os.ReadFile(filepath.Join(runtime, "operations", "op-test.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("status changed journal: %v", err)
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
