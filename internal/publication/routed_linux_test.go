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

func TestRestartRoutedDependencyClosesPublication(t *testing.T) {
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
exec sleep 60
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
	cfg := `version = 1
[project]
name = 'demo'
[services.api]
command = 'python3 -m http.server {port} --bind {host}'
port = 'auto'
[services.web]
command = 'python3 -m http.server {port} --bind {host}'
port = 'auto'
depends_on = ['api']
routes = [{ path = '/api', dependency = 'api' }]
`

	if e := os.WriteFile(filepath.Join(root, "dev.toml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	p, e := discovery.Discover(root)
	if e != nil {
		t.Fatal(e)
	}
	w := &p.Worktrees[0]
	id := p.ID + "/" + w.ID + "/web"
	apiID := p.ID + "/" + w.ID + "/api"
	store, e := state.New(filepath.Join(dir, "state"))
	if e != nil {
		t.Fatal(e)
	}
	if e = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: map[string]state.Instance{id: {ID: id, Service: "web", WorktreeID: w.ID, State: "stopped"}, apiID: {ID: apiID, Service: "api", WorktreeID: w.ID, State: "stopped"}}})
	}); e != nil {
		t.Fatal(e)
	}
	r := runner.New(store, binary)
	m := &Manager{Store: store, Executable: binary}
	selection := runner.Selection{Project: p, Worktree: w, Service: "web"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, e = r.Up(ctx, p, w, "web"); e != nil {
		t.Fatal(e)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = m.Unshare(cleanup, selection)
		_, _ = r.Stop(cleanup, p, w, "web")
		_, _ = r.Stop(cleanup, p, w, "api")
	}()
	created, e := m.Share(ctx, selection, Options{Mode: "live"})
	if e != nil {
		t.Fatal(e)
	}
	if len(created.Routed) != 1 || created.Routed[0].InstanceID != apiID {
		t.Fatalf("missing routed pin: %+v", created)
	}
	if _, e = r.Restart(ctx, p, w, "api"); e != nil {
		t.Fatal(e)
	}
	all, e := m.List(ctx)
	if e != nil || len(all) != 0 {
		t.Fatalf("publication remained after dependency restart: %+v %v", all, e)
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
		t.Fatalf("consumer stopped: %s %v", observed, e)
	}
}
