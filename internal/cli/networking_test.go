// Tests cli networking behavior.
package cli

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/networking"
	"github.com/bettertomorrow-dev/rundevrun/internal/proxy"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestNetworkPlanIsReadOnlyAndApplyUsesPlan(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	svc, err := networking.DefaultService()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := svc.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desired := snapshot.Config
	desired.Local.Listen = "127.0.0.1:9090"
	data, _ := json.Marshal(desired)
	configPath := filepath.Join(root, "desired.json")
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	e := result("network plan", options{file: configPath})
	store := &state.Store{Root: filepath.Join(root, "state")}
	if _, err := state.New(store.Root); err != nil {
		t.Fatal(err)
	}
	networkingCommand(context.Background(), store, []string{"network", "plan"}, options{file: configPath}, &e)
	if len(e.Errors) != 0 || e.Plan == nil {
		t.Fatalf("plan failed: %+v", e)
	}
	if _, err := os.Stat(svc.Root); !os.IsNotExist(err) {
		t.Fatalf("read-only plan created config root: %v", err)
	}
	planBytes, err := json.Marshal(e.Plan)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(root, "plan.json")
	if err := os.WriteFile(planPath, planBytes, 0600); err != nil {
		t.Fatal(err)
	}
	e = result("network apply", options{file: planPath})
	networkingCommand(context.Background(), store, []string{"network", "apply"}, options{file: planPath}, &e)
	if len(e.Errors) != 0 || len(e.Results) != 1 {
		t.Fatalf("apply failed: %+v", e)
	}
	updated, err := svc.Inspect(context.Background())
	if err != nil || updated.Config.Local.Listen != "127.0.0.1:9090" {
		t.Fatalf("config not applied: %+v, %v", updated.Config, err)
	}
}

func TestSetupApplyReloadsExistingProxyAndReleasesReservation(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	stateRoot, err := os.MkdirTemp("/tmp", "rdr-state-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateRoot) })
	t.Setenv("XDG_CONFIG_HOME", configHome)
	store := &state.Store{Root: stateRoot}
	if _, err := state.New(store.Root); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(root, "bin")
	if err := os.MkdirAll(fakeBin, 0700); err != nil {
		t.Fatal(err)
	}
	tailscale := filepath.Join(fakeBin, "tailscale")
	if err := os.WriteFile(tailscale, []byte("#!/bin/sh\nprintf '%s' '{\"TCP\":{}}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, source, _, _ := runtime.Caller(0)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	binary := filepath.Join(root, "rundevrun")
	build := exec.Command("go", "build", "-o", binary, "./cmd/rundevrun")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build rundevrun: %v: %s", err, output)
	}
	configPath, err := routing.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	settings := networking.DefaultConfig()
	settings.Local.Listen = listener.Addr().String()
	listener.Close()
	configRoot := filepath.Dir(configPath)
	if err := os.MkdirAll(configRoot, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "networking.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := proxy.Ensure(context.Background(), store, binary, configPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Stop(context.Background(), store) })
	waitProxyReady(t, store)

	service, err := networking.DefaultService()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.PlanSetup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plan.TermsAccepted = true
	apply := func(plan networking.SetupPlan) envelope {
		t.Helper()
		data, err := json.Marshal(plan)
		if err != nil {
			t.Fatal(err)
		}
		planPath := filepath.Join(root, "setup-plan.json")
		if err := os.WriteFile(planPath, data, 0600); err != nil {
			t.Fatal(err)
		}
		e := result("network setup-apply", options{file: planPath})
		networkingCommand(context.Background(), store, []string{"network", "setup-apply"}, options{file: planPath}, &e)
		return e
	}
	if e := apply(plan); e.Status != "ok" || len(e.Errors) != 0 {
		t.Fatalf("setup apply failed with running proxy: %+v", e)
	}
	info, err := proxy.Status(store)
	if err != nil || info.State != "ready" || info.PreparedListen != "" {
		t.Fatalf("proxy reload/reservation state after setup: %+v, %v", info, err)
	}

	plan.Baseline = "stale-baseline"
	if e := apply(plan); e.Status != "error" {
		t.Fatalf("stale setup plan was not rejected: %+v", e)
	}
	info, err = proxy.Status(store)
	if err != nil || info.State != "ready" || info.PreparedListen != "" {
		t.Fatalf("failed setup left listener reservation: %+v, %v", info, err)
	}
}

func waitProxyReady(t *testing.T, store *state.Store) {
	t.Helper()
	for range 100 {
		info, err := proxy.Status(store)
		if err == nil && info.State == "ready" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("proxy did not become ready")
}

func TestNetworkJSONRejectsSecretFieldsAndTrailingData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.json")
	for _, body := range []string{`{"version":1,"token":"secret"}`, `{} {}`} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		var plan networking.Plan
		if err := readNetworkJSON(path, &plan); err == nil {
			t.Fatalf("accepted invalid JSON: %s", body)
		}
	}
}

func TestNetworkApplyRejectsRoutePreviewFromChangedRegistry(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	service, err := networking.DefaultService()
	if err != nil {
		t.Fatal(err)
	}
	store := &state.Store{Root: filepath.Join(root, "state")}
	if _, err := state.New(store.Root); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desired := snapshot.Config
	desired.Local.Listen = "127.0.0.1:9091"
	data, _ := json.Marshal(desired)
	desiredPath := filepath.Join(root, "desired.json")
	if err := os.WriteFile(desiredPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	e := result("network plan", options{file: desiredPath})
	networkingCommand(context.Background(), store, []string{"network", "plan"}, options{file: desiredPath}, &e)
	if len(e.Errors) != 0 {
		t.Fatalf("plan failed: %+v", e)
	}
	planBytes, _ := json.Marshal(e.Plan)
	planPath := filepath.Join(root, "plan.json")
	if err := os.WriteFile(planPath, planBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(*state.Registry) error { return nil }); err != nil {
		t.Fatal(err)
	}
	e = result("network apply", options{file: planPath})
	networkingCommand(context.Background(), store, []string{"network", "apply"}, options{file: planPath}, &e)
	if len(e.Errors) == 0 || !strings.Contains(e.Errors[0].Message, "route candidates changed") {
		t.Fatalf("stale route preview was accepted: %+v", e)
	}
	after, err := service.Inspect(context.Background())
	if err != nil || after.Config.Local.Listen != "127.0.0.1:8080" {
		t.Fatalf("stale route preview changed networking settings: %+v, %v", after.Config, err)
	}
}
