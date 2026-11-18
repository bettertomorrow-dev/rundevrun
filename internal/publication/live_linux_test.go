//go:build linux

// Tests publication live behavior on Linux.
package publication

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
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

func TestOfflineLiveLifecycle(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "rundevrun")
	if b, e := exec.Command("go", "build", "-o", binary, "../../cmd/rundevrun").CombinedOutput(); e != nil {
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
if [ "$FAKE_BLOCK_OLD_STOP" = 1 ]; then
 trap 'touch "$FAKE_OLD_STOP_MARKER"; while [ ! -f "$FAKE_OLD_STOP_RELEASE" ]; do sleep 0.01; done; exit 0' TERM
fi
printf '%s\n' 'INF Registered tunnel connection'
if [ "$FAKE_BLOCK_OLD_STOP" = 1 ]; then
 while :; do sleep 60 & wait $!; done
fi
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
	snapshotConfig := cfg + "[services.api.share]\nmode = 'snapshot'\nbuild = 'mkdir -p dist; printf secret-build-command > dist/index.html'\ndir = 'dist'\nenv = { PRIVATE_VALUE = 'snapshot-secret-value' }\n"
	if e = os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(snapshotConfig), 0600); e != nil {
		t.Fatal(e)
	}
	snapshotPlan, e := m.PlanSnapshot(ctx, selection)
	if e != nil {
		t.Fatal(e)
	}
	encodedPlan, e := json.Marshal(snapshotPlan)
	if e != nil || strings.Contains(string(encodedPlan), "secret-build-command") || strings.Contains(string(encodedPlan), "snapshot-secret-value") {
		t.Fatalf("snapshot plan leaked private build: %s %v", encodedPlan, e)
	}
	if snapshotPlan.Digest == "" || snapshotPlan.ArtifactDir != "dist" {
		t.Fatalf("invalid snapshot plan: %+v", snapshotPlan)
	}
	configured, e := m.Plan(ctx, selection, Options{})
	if e != nil || configured.Mode != "snapshot" || configured.Snapshot == nil {
		t.Fatalf("configured mode: %+v %v", configured, e)
	}
	override, e := m.Plan(ctx, selection, Options{Mode: "live"})
	if e != nil || override.Mode != "live" || override.Live == nil {
		t.Fatalf("mode override: %+v %v", override, e)
	}
	snapshotResult, e := m.Share(ctx, selection, Options{})
	if e != nil {
		t.Fatal(e)
	}
	if snapshotResult.Mode != "snapshot" || snapshotResult.Password == "" {
		t.Fatalf("snapshot result: %+v", snapshotResult)
	}
	copied, e := os.ReadFile(filepath.Join(store.Root, "publications", snapshotResult.ID, "snapshot", "index.html"))
	if e != nil || string(copied) != "secret-build-command" {
		t.Fatalf("snapshot copy: %q %v", copied, e)
	}
	if e = os.RemoveAll(filepath.Join(root, "dist")); e != nil {
		t.Fatal(e)
	}
	gatewayBytes, e := os.ReadFile(gatewayFile)
	if e != nil {
		t.Fatal(e)
	}
	snapshotClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	loginForm := url.Values{"password": {snapshotResult.Password}}
	login, e := http.NewRequest("POST", string(gatewayBytes)+"/_rdr/login", strings.NewReader(loginForm.Encode()))
	if e != nil {
		t.Fatal(e)
	}
	login.Host = "example.trycloudflare.com"
	login.Header.Set("X-Forwarded-Proto", "https")
	login.Header.Set("Origin", "https://example.trycloudflare.com")
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginResponse, e := snapshotClient.Do(login)
	if e != nil {
		t.Fatal(e)
	}
	loginResponse.Body.Close()
	if len(loginResponse.Cookies()) == 0 {
		t.Fatal("snapshot login missing cookie")
	}
	asset, e := http.NewRequest("GET", string(gatewayBytes)+"/", nil)
	if e != nil {
		t.Fatal(e)
	}
	asset.Host = "example.trycloudflare.com"
	asset.Header.Set("X-Forwarded-Proto", "https")
	asset.AddCookie(loginResponse.Cookies()[0])
	assetResponse, e := snapshotClient.Do(asset)
	if e != nil {
		t.Fatal(e)
	}
	assetData, e := io.ReadAll(assetResponse.Body)
	assetResponse.Body.Close()
	if e != nil || assetResponse.StatusCode != 200 || string(assetData) != "secret-build-command" {
		t.Fatalf("snapshot gateway: %d %q %v", assetResponse.StatusCode, assetData, e)
	}
	for _, action := range []string{"down", "restart"} {
		plan, err := r.PlanGraph(ctx, action, []runner.Selection{selection}, false)
		if err != nil || len(plan.Publications) != 1 || plan.Publications[0] != snapshotResult.ID {
			t.Fatalf("%s publication plan: %+v %v", action, plan.Publications, err)
		}
	}
	brokenConfig := strings.Replace(snapshotConfig, "mkdir -p dist; printf secret-build-command > dist/index.html", "exit 7", 1)
	if e = os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(brokenConfig), 0600); e != nil {
		t.Fatal(e)
	}
	failed, buildErr := m.Share(ctx, selection, Options{Mode: "snapshot", Recreate: true})
	var typed *runner.Error
	if !errors.As(buildErr, &typed) || typed.Code != "BUILD_FAILED" || failed.ID == "" || failed.State != "failed" || failed.Password != "" || len(failed.LogPaths) == 0 {
		t.Fatalf("broken snapshot diagnostics: %+v %v", failed, buildErr)
	}
	buildCause := false
	for _, path := range failed.LogPaths {
		if !strings.HasSuffix(path, "build.log") && !strings.HasSuffix(path, "tunnel.log") {
			t.Fatalf("unsafe log path: %s", path)
		}
		if strings.HasSuffix(path, "build.log") {
			data, err := os.ReadFile(path)
			buildCause = err == nil && strings.Contains(string(data), "exit status 7")
		}
	}
	if !buildCause {
		t.Fatal("failed publication build log omitted the command failure cause")
	}
	still, e := m.Status(ctx, selection)
	if e != nil || still.ID != snapshotResult.ID || still.State != "ready" {
		t.Fatalf("old snapshot lost: %+v %v", still, e)
	}
	buildPID := filepath.Join(dir, "snapshot-build.pid")
	longConfig := strings.Replace(snapshotConfig, "mkdir -p dist; printf secret-build-command > dist/index.html", "echo $$ > "+buildPID+"; sleep 60", 1)
	if e = os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(longConfig), 0600); e != nil {
		t.Fatal(e)
	}
	short, stopBuild := context.WithTimeout(ctx, 2*time.Second)
	_, e = m.Share(short, selection, Options{Mode: "snapshot", Recreate: true})
	stopBuild()
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("snapshot cancellation: %v", e)
	}
	pidText, e := os.ReadFile(buildPID)
	if e != nil {
		t.Fatal(e)
	}
	pid, e := strconv.Atoi(strings.TrimSpace(string(pidText)))
	if e != nil {
		t.Fatal(e)
	}
	if ref, err := procid.Observe(pid); err == nil {
		if live, _ := procid.Check(ref); live {
			t.Fatalf("snapshot build survived cancellation: %d", pid)
		}
	}
	if e = os.RemoveAll(filepath.Join(root, "dist")); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(store.Root, "publications", snapshotResult.ID, "snapshot", "index.html")); e != nil {
		t.Fatal(e)
	}
	if _, e = m.Unshare(ctx, selection); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(root, "rundevrun.toml"), []byte(cfg), 0600); e != nil {
		t.Fatal(e)
	}
	type shareResponse struct {
		result Result
		err    error
	}
	done := make(chan shareResponse, 1)
	go func() { v, err := m.Share(ctx, selection, Options{Mode: "live"}); done <- shareResponse{v, err} }()
	for n := 0; n < 200; n++ {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("tunnel URL not announced")
	}
	select {
	case early := <-done:
		t.Fatalf("share returned before connection: %+v %v", early.result, early.err)
	case <-time.After(100 * time.Millisecond):
	}
	completed := <-done
	created, e := completed.result, completed.err
	if e != nil {
		t.Fatal(e)
	}
	if created.Password == "" || created.URL != "https://example.trycloudflare.com" || created.State != "ready" {
		t.Fatalf("created: %+v", created)
	}
	data, e := os.ReadFile(envFile)
	if e != nil || len(data) != 0 {
		t.Fatalf("credential env leaked: %q %v", data, e)
	}
	data, e = os.ReadFile(gatewayFile)
	if e != nil {
		t.Fatal(e)
	}
	gateway := string(data)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(method, path string, body io.Reader) *http.Request {
		req, err := http.NewRequest(method, gateway+path, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "example.trycloudflare.com"
		req.Header.Set("X-Forwarded-Proto", "https")
		return req
	}
	response, e := client.Do(request("GET", "/", nil))
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauth status %d", response.StatusCode)
	}
	form := url.Values{"password": {created.Password}}
	req := request("POST", "/_rdr/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://example.trycloudflare.com")
	response, e = client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	cookies := response.Cookies()
	if len(cookies) == 0 {
		t.Fatal("missing session cookie")
	}
	req = request("GET", "/", nil)
	req.AddCookie(cookies[0])
	response, e = client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authorized status %d", response.StatusCode)
	}
	privateData, err := os.ReadFile(filepath.Join(store.Root, "publications.json"))
	if err != nil || strings.Contains(string(privateData), created.Password) {
		t.Fatalf("plaintext password persisted: %v", err)
	}
	publicList, err := m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encodedList, err := json.Marshal(publicList)
	if err != nil || strings.Contains(string(encodedList), created.Password) || strings.Contains(string(encodedList), "signing_key") || strings.Contains(string(encodedList), "verifier") {
		t.Fatalf("status leaked private data: %s %v", encodedList, err)
	}
	encoded, err := json.Marshal(created)
	if err != nil || strings.Contains(string(encoded), created.Password) {
		t.Fatalf("password leaked through JSON: %s %v", encoded, err)
	}
	again, e := m.Share(ctx, selection, Options{Mode: "live"})
	if e != nil || again.ID != created.ID || again.Password != "" {
		t.Fatalf("repeat: %+v %v", again, e)
	}
	if e = os.WriteFile(failFile, []byte("fail"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = m.Share(ctx, selection, Options{Mode: "live", Recreate: true}); e == nil {
		t.Fatal("failed recreate succeeded")
	}
	status, e := m.Status(ctx, selection)
	if e != nil || status.ID != created.ID || status.State != "ready" {
		t.Fatalf("failed recreate lost old: %+v %v", status, e)
	}
	if e = os.Remove(failFile); e != nil {
		t.Fatal(e)
	}
	recreated, e := m.Share(ctx, selection, Options{Mode: "live", Recreate: true})
	if e != nil || recreated.ID == created.ID || recreated.Password == "" || recreated.Password == created.Password {
		t.Fatalf("recreate: %+v %v", recreated, e)
	}
	data, e = os.ReadFile(gatewayFile)
	if e != nil {
		t.Fatal(e)
	}
	gateway = string(data)
	req = request("GET", "/", nil)
	req.AddCookie(cookies[0])
	response, e = client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("old cookie accepted after recreate: %d", response.StatusCode)
	}
	lock, e := store.LockPublicationRegistry()
	if e != nil {
		t.Fatal(e)
	}
	pubs, e := store.ReadPublications()
	if e != nil {
		lock.Close()
		t.Fatal(e)
	}
	pending := pubs.Publications[recreated.ID]
	pending.ID = "abandoned-pending"
	pending.Phase = "preparing"
	pending.PreviousID = recreated.ID
	pubs.Publications[pending.ID] = pending
	pubs.Revision++
	e = store.WritePublications(pubs)
	lock.Close()
	if e != nil {
		t.Fatal(e)
	}
	all, e := m.List(ctx)
	if e != nil || len(all) != 2 {
		t.Fatalf("pending publication hidden: %+v %v", all, e)
	}
	if _, e = m.Unshare(ctx, selection); e != nil {
		t.Fatal(e)
	}
	pubs, e = store.ReadPublications()
	if e != nil || pubs.Publications[pending.ID].Phase != "closed" || pubs.Publications[recreated.ID].Phase != "closed" {
		t.Fatalf("unshare left candidate: %+v %v", pubs, e)
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
		t.Fatalf("app after unshare: %s %v", observed, e)
	}

	stopMarker, stopRelease := filepath.Join(dir, "old-stopping"), filepath.Join(dir, "old-stop-release")
	t.Setenv("FAKE_OLD_STOP_MARKER", stopMarker)
	t.Setenv("FAKE_OLD_STOP_RELEASE", stopRelease)
	t.Setenv("FAKE_BLOCK_OLD_STOP", "1")
	old, e := m.Share(ctx, selection, Options{Mode: "live"})
	if e != nil {
		t.Fatal(e)
	}
	t.Setenv("FAKE_BLOCK_OLD_STOP", "")
	go func() {
		v, err := m.Share(ctx, selection, Options{Mode: "live", Recreate: true})
		done <- shareResponse{v, err}
	}()
	for {
		if _, err := os.Stat(stopMarker); err == nil {
			break
		}
		select {
		case early := <-done:
			t.Fatalf("recreate returned before old shutdown: %+v %v", early.result.Status, early.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	registryLock, e := store.LockPublicationRegistry()
	if e != nil {
		t.Fatal(e)
	}
	defer registryLock.Close()
	if e = os.WriteFile(stopRelease, nil, 0600); e != nil {
		t.Fatal(e)
	}
	completed = <-done
	active, cleanupErr := completed.result, completed.err
	if cleanupErr == nil || active.ID == "" || active.ID == old.ID || active.State != "ready" || active.Password != "" {
		t.Fatalf("replacement after cleanup failure: %+v %v", active.Status, cleanupErr)
	}
	if e = registryLock.Close(); e != nil {
		t.Fatal(e)
	}
	pubs, e = store.ReadPublications()
	if e != nil || pubs.Publications[active.ID].Phase != "active" || pubs.Publications[old.ID].Phase != "closing" {
		t.Fatalf("replacement was not committed before old shutdown: %v", e)
	}
	runtime, e := store.ReadPublicationRuntime(active.ID)
	if e != nil || runtime.State != "ready" {
		t.Fatalf("replacement was rolled back after activation: %v", e)
	}
	if _, e = m.Unshare(ctx, selection); e != nil {
		t.Fatalf("explicit recovery after cleanup failure: %v", e)
	}
}
