// Exercises crash recovery using the installed command interface.
package acceptance

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestCrashRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs rundevrun")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(base, "rdr")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	t.Setenv("RDR_HOME", filepath.Join(base, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	run := func(ok bool, args ...string) string {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Dir = base
		out, err := cmd.CombinedOutput()
		if (err == nil) != ok {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git := exec.Command("git", "init", "-q", "-b", "main", base)
	if out, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	fixture, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := strconv.Quote("exec '" + strings.ReplaceAll(fixture, "'", "'\\''") + "' -test.run=^TestPoCHTTPHelper$")
	listener, port := reservePort(t)
	listener.Close()
	config := fmt.Sprintf(`version = 1
[project]
name = "recovery"
[services.source]
command = %s
port = "auto"
protocol = "tcp"
health = { tcp = true }
env = { POC_HTTP_HELPER = "1" }
restart = "on-failure"
restart_delay = "1s"
restart_stable_after = "500ms"
restart_consumers = true
[services.worker]
command = "exec sleep 60"
depends_on = ["source"]
[services.front]
command = "exec sleep 60"
depends_on = ["worker", "source"]
[services.idle]
command = "exec sleep 60"
depends_on = ["source"]
[services.api]
command = %s
port = %d
protocol = "tcp"
health = { tcp = true }
env = { POC_HTTP_HELPER = "1" }
[services.conflict]
command = %s
port = %d
protocol = "tcp"
env = { POC_HTTP_HELPER = "1" }
[services.flap]
command = "exit 7"
restart = "on-failure"
start_timeout = "1s"
restart_delay = "400ms"
restart_max_attempts = 2
`, command, command, port, command, port)
	if err := os.WriteFile(filepath.Join(base, "rundevrun.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd := exec.Command(binary, "down", "--all")
		cmd.Dir = base
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("cleanup: %v\n%s", err, out)
		}
	})
	type instance struct {
		Service  string      `json:"service"`
		State    string      `json:"state"`
		RunID    string      `json:"run_id"`
		PID      int         `json:"pid"`
		Restarts int         `json:"restarts"`
		LastExit *state.Exit `json:"last_exit"`
	}
	status := func() map[string]instance {
		t.Helper()
		var result struct{ Results []instance }
		if err := json.Unmarshal([]byte(run(true, "status", "--json")), &result); err != nil {
			t.Fatal(err)
		}
		instances := map[string]instance{}
		for _, item := range result.Results {
			instances[item.Service] = item
		}
		return instances
	}
	wait := func(check func(map[string]instance) bool) map[string]instance {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			current := status()
			if check(current) {
				return current
			}
			if time.Now().After(deadline) {
				t.Fatalf("unexpected status after waiting: %+v", current)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	kill := func(pid int) {
		t.Helper()
		if pid <= 1 {
			t.Fatalf("invalid PID %d", pid)
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
	}

	run(true, "up", "front", "api", "--no-proxy")
	before := status()
	conflict := run(false, "up", "conflict", "--json", "--no-proxy")
	if !strings.Contains(conflict, before["api"].RunID) || !strings.Contains(conflict, "rundevrun down api") {
		t.Fatalf("missing conflict owner or recovery action: %s", conflict)
	}
	kill(before["source"].PID)
	after := wait(func(current map[string]instance) bool {
		return current["source"].State == "ready" && current["source"].Restarts == 1 &&
			current["front"].State == "running" && current["front"].RunID != before["front"].RunID
	})
	if after["source"].RunID != before["source"].RunID || after["worker"].RunID == before["worker"].RunID ||
		after["idle"].State != "stopped" || after["source"].LastExit == nil || after["source"].LastExit.Signal == "" {
		t.Fatalf("unexpected recovery: %+v", after)
	}
	time.Sleep(500 * time.Millisecond)
	kill(after["source"].PID)
	wait(func(current map[string]instance) bool { return current["source"].State == "restarting" })
	if out := run(true, "logs", "source"); strings.Count(out, "restarting in 1s (attempt 1)") != 2 {
		t.Fatalf("backoff did not reset after the stable interval: %s", out)
	}
	run(true, "down", "source", "--cascade")
	time.Sleep(1100 * time.Millisecond)
	if item := status()["source"]; item.State != "stopped" || item.Restarts != 1 {
		t.Fatalf("retry ran after stop: %+v", item)
	}

	kill(before["api"].PID)
	failed := wait(func(current map[string]instance) bool { return current["api"].State == "failed" })["api"]
	if failed.LastExit == nil || failed.LastExit.Signal == "" || failed.LastExit.Time.IsZero() {
		t.Fatalf("missing exit details: %+v", failed)
	}
	if out := run(true, "logs", "api"); !strings.Contains(out, "[rundevrun]") || !strings.Contains(out, failed.LastExit.Signal) {
		t.Fatalf("missing exit marker: %s", out)
	}
	run(true, "up", "api", "--no-proxy")
	api := status()["api"]
	store := &state.Store{Root: os.Getenv("RDR_HOME")}
	record, err := store.ReadRun(api.RunID)
	if err != nil {
		t.Fatal(err)
	}
	kill(record.Supervisor.PID)
	kill(api.PID)
	deadline := time.Now().Add(3 * time.Second)
	for procid.Matches(record.Supervisor) || procid.Matches(record.Child) {
		if time.Now().After(deadline) {
			t.Fatal("processes survived SIGKILL")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out := run(true, "logs", "api"); !strings.Contains(out, "supervisor disappeared") {
		t.Fatalf("missing supervisor loss marker: %s", out)
	}
	run(true, "up", "api", "--no-proxy")
	if record, err = store.ReadRun(api.RunID); err != nil || record.State != "failed" || record.LastExit == nil {
		t.Fatalf("failed to reconcile stale supervisor record: %+v %v", record, err)
	}
	run(false, "up", "flap", "--no-proxy")
	flap := status()["flap"]
	if flap.State != "failed" || flap.Restarts != 2 || flap.LastExit == nil || flap.LastExit.Code == nil || *flap.LastExit.Code != 7 {
		t.Fatalf("retry limit or exit code: %+v", flap)
	}
}
