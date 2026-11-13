//go:build linux

package publication

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestTunnelFailureLeavesAppRunning(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rdr")
	if b, e := exec.Command("go", "build", "-o", binary, "../../cmd/rdr").CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	fakeDir := filepath.Join(dir, "bin")
	if e := os.Mkdir(fakeDir, 0700); e != nil {
		t.Fatal(e)
	}
	gatewayFile := filepath.Join(dir, "gateway")
	envFile := filepath.Join(dir, "env")
	marker := filepath.Join(dir, "url-announced")
	failFile := filepath.Join(dir, "fail-tunnel")
	fake := `#!/bin/sh
if [ -f "$FAKE_FAIL_FILE" ]; then exit 1; fi
while [ "$#" -gt 0 ]; do
 if [ "$1" = "--url" ]; then shift; printf '%s' "$1" > "$FAKE_GATEWAY_FILE"; fi
 shift
done
env | grep -E '^(TUNNEL_|CLOUDFLARED_|CF_)' > "$FAKE_ENV_FILE" || true
printf '%s\n' 'https://example.trycloudflare.com'
touch "$FAKE_URL_MARKER"
sleep 0.6
printf '%s\n' 'INF Registered tunnel connection'
sleep 0.4
exit 1
`
	if e := os.WriteFile(filepath.Join(fakeDir, "cloudflared"), []byte(fake), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_GATEWAY_FILE", gatewayFile)
	t.Setenv("FAKE_ENV_FILE", envFile)
	t.Setenv("FAKE_URL_MARKER", marker)
	t.Setenv("FAKE_FAIL_FILE", failFile)
	t.Setenv("TUNNEL_TOKEN", "must-not-inherit")
	root := filepath.Join(dir, "project")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := "version = 1\n[project]\nname = 'demo'\n[services.api]\ncommand = 'python3 -m http.server {port} --bind {host}'\nport = 'auto'\n"
	if e := os.WriteFile(filepath.Join(root, "dev.toml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	p, e := discovery.Discover(root)
	if e != nil {
		t.Fatal(e)
	}
	w := &p.Worktrees[0]
	id := p.ID + "/" + w.ID + "/api"
	store, e := state.New(filepath.Join(dir, "state"))
	if e != nil {
		t.Fatal(e)
	}
	if e = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: map[string]state.Instance{id: {ID: id, Service: "api", WorktreeID: w.ID, State: "stopped"}}})
	}); e != nil {
		t.Fatal(e)
	}
	r := runner.New(store, binary)
	m := &Manager{Store: store, Executable: binary}
	selection := runner.Selection{Project: p, Worktree: w, Service: "api"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, e = r.Up(ctx, p, w, "api"); e != nil {
		t.Fatal(e)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = m.Unshare(cleanup, selection)
		_, _ = r.Stop(cleanup, p, w, "api")
	}()
	created, e := m.Share(ctx, selection, Options{Mode: "live"})
	if e != nil {
		t.Fatal(e)
	}
	var status Status
	for n := 0; n < 300; n++ {
		status, e = m.Status(ctx, selection)
		if e == nil && status.State == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e != nil || status.State != "failed" {
		t.Fatalf("tunnel crash not exposed: %+v %v", status, e)
	}
	if _, e = m.Unshare(ctx, selection); e != nil {
		t.Fatal(e)
	}
	reg, e := store.Read()
	if e != nil {
		t.Fatal(e)
	}
	project, e := reg.Project(p.ID)
	if e != nil {
		t.Fatal(e)
	}
	observed, e := store.Inspect(project.Instances[id])
	if e != nil || observed != "running" {
		t.Fatalf("app after tunnel failure: %s %v (publication %s)", observed, e, created.ID)
	}
}
