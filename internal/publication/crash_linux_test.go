//go:build linux

// Tests publication crash behavior on Linux.
package publication

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestPublicationFrontendChild(t *testing.T) {
	pos := -1
	for i, v := range os.Args {
		if v == "--" {
			pos = i
			break
		}
	}
	if pos < 0 || len(os.Args) != pos+4 {
		t.Skip("frontend helper")
	}
	root, stateRoot, binary := os.Args[pos+1], os.Args[pos+2], os.Args[pos+3]
	p, e := discovery.Discover(root)
	if e != nil {
		t.Fatal(e)
	}
	_, e = (&Manager{Store: &state.Store{Root: stateRoot}, Executable: binary}).Share(context.Background(), runner.Selection{Project: p, Worktree: &p.Worktrees[0], Service: "api"}, Options{Mode: os.Getenv("FAKE_FRONTEND_MODE")})
	if e != nil {
		t.Fatal(e)
	}
}
func TestKilledFrontendBeforeActivationCanUnshare(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rundevrun")
	if b, e := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun").CombinedOutput(); e != nil {
		t.Fatalf("build: %v %s", e, b)
	}
	fakeDir := filepath.Join(dir, "bin")
	if e := os.Mkdir(fakeDir, 0700); e != nil {
		t.Fatal(e)
	}
	marker := filepath.Join(dir, "announced")
	allow := filepath.Join(dir, "allow")
	fake := `#!/bin/sh
printf '%s\n' 'https://example.trycloudflare.com'
touch "$FAKE_MARKER"
while [ ! -f "$FAKE_ALLOW" ]; do sleep 0.02; done
printf '%s\n' 'INF Registered tunnel connection'
exec sleep 60
`
	if e := os.WriteFile(filepath.Join(fakeDir, "cloudflared"), []byte(fake), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_MARKER", marker)
	t.Setenv("FAKE_ALLOW", allow)
	root := filepath.Join(dir, "project")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := isolatedHTTPPorts(t, "version = 1\n[project]\nname = 'demo'\n[services.api]\ncommand = 'python3 -m http.server {port} --bind {host}'\nport = 'auto'\n")
	if e := os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(cfg), 0600); e != nil {
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
	sel := runner.Selection{Project: p, Worktree: w, Service: "api"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, e = r.Up(ctx, p, w, "api"); e != nil {
		t.Fatal(e)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = m.Unshare(cleanup, sel)
		_, _ = r.Stop(cleanup, p, w, "api")
	}()
	child := exec.Command(os.Args[0], "-test.run=^TestPublicationFrontendChild$", "--", root, store.Root, binary)
	if e = child.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if child.Process != nil {
			_ = child.Process.Kill()
			_, _ = child.Process.Wait()
		}
	}()
	for n := 0; n < 300; n++ {
		if _, e = os.Stat(marker); e == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e != nil {
		t.Fatal("tunnel did not announce URL")
	}
	lock, e := store.LockPublicationRegistry()
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Close()
	if e = os.WriteFile(allow, []byte("go"), 0600); e != nil {
		t.Fatal(e)
	}
	var runtime state.PublicationRuntime
	var publicationID string
	for n := 0; n < 300; n++ {
		registry, err := store.ReadPublications()
		if err == nil {
			for id, pub := range registry.Publications {
				if pub.Phase == "preparing" {
					rt, err := store.ReadPublicationRuntime(id)
					if err == nil && rt.State == "ready" {
						runtime = rt
						publicationID = id
						break
					}
				}
			}
		}
		if publicationID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if publicationID == "" {
		t.Fatal("publication did not reach ready boundary")
	}
	if e = child.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = child.Wait()
	child.Process = nil
	if e = lock.Close(); e != nil {
		t.Fatal(e)
	}
	all, e := m.List(ctx)
	if e != nil || len(all) != 1 || all[0].ID != publicationID || all[0].State != "failed" || all[0].URL != "" {
		t.Fatalf("dead owner with ready runtime must be interrupted: %+v %v", all, e)
	}
	for n := 0; n < 300; n++ {
		live, err := procid.Check(runtime.Supervisor)
		if err != nil {
			t.Fatal(err)
		}
		if !live {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	all, e = m.List(ctx)
	if e != nil || len(all) != 1 || all[0].ID != publicationID || all[0].State != "failed" {
		t.Fatalf("interrupted publication hidden: %+v %v", all, e)
	}
	if _, e = m.Share(ctx, sel, Options{Mode: "live"}); e == nil {
		t.Fatal("new share ignored interrupted publication")
	}
	if _, e = m.Unshare(ctx, sel); e != nil {
		t.Fatal(e)
	}
	all, e = m.List(ctx)
	if e != nil || len(all) != 0 {
		t.Fatalf("unshare left publication: %+v %v", all, e)
	}
	live, e := procid.Check(runtime.Tunnel)
	if e != nil || live {
		t.Fatalf("tunnel survived: %v %v", live, e)
	}
}

func TestKilledFrontendDuringSnapshotBuild(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rundevrun")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\nprintf '%s\\n' 'https://example.trycloudflare.com' 'INF Registered tunnel connection'\nexec sleep 60\n"
	if err := os.WriteFile(filepath.Join(bin, "cloudflared"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_FRONTEND_MODE", "snapshot")
	root := filepath.Join(dir, "project")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "building")
	cfg := isolatedHTTPPorts(t, "version = 1\n[project]\nname = 'demo'\n[services.api]\ncommand = 'python3 -m http.server {port} --bind {host}'\nport = 'auto'\n[services.api.share]\nmode = 'snapshot'\nbuild = 'sleep 60 & echo $! > "+marker+"; wait'\ndir = 'dist'\n")
	if err := os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := discovery.Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	w := &p.Worktrees[0]
	id := p.ID + "/" + w.ID + "/api"
	store, err := state.New(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(p.ID, state.Project{ID: p.ID, Name: p.Name, Root: p.Root, Worktrees: []state.Worktree{{ID: w.ID, Root: w.Root}}, Instances: map[string]state.Instance{id: {ID: id, Service: "api", WorktreeID: w.ID, State: "stopped"}}})
	}); err != nil {
		t.Fatal(err)
	}
	r := runner.New(store, binary)
	m := &Manager{Store: store, Executable: binary}
	sel := runner.Selection{Project: p, Worktree: w, Service: "api"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err = r.Up(ctx, p, w, "api"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = m.Unshare(cleanup, sel)
		_, _ = r.Stop(cleanup, p, w, "api")
	}()
	child := exec.Command(os.Args[0], "-test.run=^TestPublicationFrontendChild$", "--", root, store.Root, binary)
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if child.Process != nil {
			_ = child.Process.Kill()
			_, _ = child.Process.Wait()
		}
	}()
	for n := 0; n < 500; n++ {
		if _, err = os.Stat(marker); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal("build did not start")
	}
	pubs, err := store.ReadPublications()
	if err != nil {
		t.Fatal(err)
	}
	var idPub string
	var rt state.PublicationRuntime
	for key, pub := range pubs.Publications {
		if pub.Phase == "preparing" {
			idPub = key
			rt, err = store.ReadPublicationRuntime(key)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if idPub == "" || rt.Build.PID <= 0 {
		t.Fatalf("missing durable build: %+v", rt)
	}
	childBytes, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(childBytes)))
	if err != nil {
		t.Fatal(err)
	}
	buildChild, err := procid.Observe(childPID)
	if err != nil {
		t.Fatal(err)
	}

	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	child.Process = nil
	for n := 0; n < 500; n++ {
		live, e := procid.Check(rt.Supervisor)
		if e != nil {
			t.Fatal(e)
		}
		if !live {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = m.Unshare(ctx, sel); err != nil {
		t.Fatal(err)
	}
	live, err := procid.Check(rt.Build)
	if err != nil || live {
		t.Fatalf("build survived frontend death: %v %v", live, err)
	}
	childLive, err := procid.Check(buildChild)
	if err != nil || childLive {
		t.Fatalf("build descendant survived frontend death: %v %v", childLive, err)
	}
	all, err := m.List(ctx)
	if err != nil || len(all) != 0 {
		t.Fatalf("orphan publication: %+v %v", all, err)
	}
}
