//go:build darwin || linux

// Tests acceptance poc behavior.
package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/monitor"
	"github.com/bettertomorrow-dev/rundevrun/internal/networking"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestPoCHTTPHelper(t *testing.T) {
	if os.Getenv("POC_HTTP_HELPER") != "1" {
		return
	}
	fmt.Println("poc HTTP service")
	worktree, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	err = http.ListenAndServe("127.0.0.1:"+os.Getenv("RDR_PORT"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "poc ready", os.Getenv("POC_ID"), worktree, r.URL.RequestURI())
	}))
	t.Fatal(err)
}

func TestLaunchURLHelper(t *testing.T) {
	if os.Getenv("POC_LAUNCH_TEST") != "1" {
		return
	}
	env := map[string]string{}
	for _, key := range []string{"RDR_URL", "OWN_VALUE", "RDR_DEP_API_URL", "DEP_VALUE"} {
		env[key] = os.Getenv(key)
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("MARKER"), b, 0600); err != nil {
		t.Fatal(err)
	}
	switch os.Getenv("REPORT_MODE") {
	case "owned":
		fmt.Printf("app: url=http://127.0.0.1:%s/s/session/?x=1#frag\n", os.Getenv("RDR_PORT"))
	case "external":
		fmt.Println("app: url=https://outside.example/s/external/?token=public#external")
	}
	if err := http.ListenAndServe("127.0.0.1:"+os.Getenv("RDR_PORT"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ready")
	})); err != nil {
		t.Fatal(err)
	}
}

func reservePort(t *testing.T) (net.Listener, int) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	return listener, listener.Addr().(*net.TCPAddr).Port
}

func TestProjectsWorktreesAndOrphanProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs real rdr service processes")
	}

	tmp, err := os.MkdirTemp("", "rdr-accept-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	base, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	_, source, _, _ := runtime.Caller(0)
	repo := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	binary := filepath.Join(base, "rdr")
	build := exec.Command("go", "build", "-o", binary, "./cmd/rdr")
	build.Dir = repo
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build rdr: %v\n%s", err, out)
	}
	t.Setenv("RDR_HOME", filepath.Join(base, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	listener, proxyPort := reservePort(t)
	firstPortLock, firstPort := reservePort(t)
	featurePortLock, featurePort := reservePort(t)
	secondPortLock, secondPort := reservePort(t)
	configRoot := filepath.Join(base, "config", "rundevrun")
	if err := os.MkdirAll(configRoot, 0700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`{"version":1,"local":{"listen":"127.0.0.1:%d"},"tailnet":{}}`, proxyPort)
	if err := os.WriteFile(filepath.Join(configRoot, "networking.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	store := &state.Store{Root: os.Getenv("RDR_HOME")}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	invoke := func(dir string, success bool, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, append(args, "--json")...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if (err == nil) != success || !json.Valid(out) {
			t.Fatalf("rdr %v: err=%v output=%s", args, err, out)
		}
	}
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	fixtureBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := "'" + strings.ReplaceAll(fixtureBinary, "'", "'\\''") + "' -test.run=^TestPoCHTTPHelper$"
	writeConfig := func(dir, name string, port int) {
		t.Helper()
		config := fmt.Sprintf(`version = 1
[project]
name = %q
[services.web]
command = %s
port = %d
protocol = "http"
start_timeout = "10s"
stop_timeout = "3s"
health = { http = "/" }
env = { POC_HTTP_HELPER = "1", POC_ID = %q }
[services.broken]
command = "sleep 60"
build = "printf 'poc build failure\\n'; exit 1"
`, name, strconv.Quote(command), port, name)
		if err := os.WriteFile(filepath.Join(dir, "dev.toml"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
	}
	first, second, feature := filepath.Join(base, "first"), filepath.Join(base, "second"), filepath.Join(base, "feature")
	for _, root := range []string{first, second} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(first, "first", firstPort)
	writeConfig(second, "second", secondPort)
	git(first, "init", "-b", "main")
	git(first, "add", "dev.toml")
	git(first, "-c", "user.name=PoC", "-c", "user.email=poc@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
	git(first, "worktree", "add", "-b", "feature", feature)
	writeConfig(feature, "first", featurePort)

	invoke(first, true, "up", "web", "--dry-run", "--no-proxy")
	if _, err := os.Stat(store.Root); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote state: %v", err)
	}

	t.Cleanup(func() {
		cmd := exec.Command(binary, "down", "--all", "--json")
		cmd.Dir = base
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("cleanup down: %v %s", err, out)
		}
		cmd = exec.Command(binary, "proxy", "stop", "--json")
		cmd.Dir = base
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("cleanup proxy: %v %s", err, out)
		}
		reg, _ := store.Read()
		for id := range reg.Projects {
			p, _ := reg.Project(id)
			for _, inst := range p.Instances {
				run, err := store.ReadRun(inst.RunID)
				if err == nil {
					refs := append([]procid.Ref{run.Supervisor, run.Child}, run.Members...)
					procid.Terminate(refs, time.Second)
				}
			}
		}
	})
	listener.Close()
	for _, fixture := range []struct {
		dir  string
		port net.Listener
	}{{first, firstPortLock}, {feature, featurePortLock}, {second, secondPortLock}} {
		fixture.port.Close()
		invoke(fixture.dir, true, "up", "web", "--no-proxy")
	}
	snapshot, err := monitor.Read(ctx, store, base)
	if err != nil || len(snapshot.Services) != 3 || snapshot.CurrentProjectID != "" {
		t.Fatalf("global snapshot: %+v err=%v", snapshot, err)
	}
	var orphan, secondService, mainService monitor.Service
	for _, svc := range snapshot.Services {
		if svc.PID <= 0 || len(svc.Ports) == 0 || svc.URL == "" {
			t.Fatalf("missing live details: %+v", svc)
		}
		client := &http.Client{Timeout: time.Second}
		response, err := client.Get(svc.DirectURL)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		local, err := url.Parse(svc.LocalURL)
		if err != nil || local.Hostname() == "" {
			t.Fatalf("missing local URL for %s: %q: %v", svc.ID, svc.LocalURL, err)
		}
		req, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/path?q=1", proxyPort), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = local.Host
		response, err = client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || string(body) != "poc ready "+svc.ProjectName+" "+svc.Worktree+" /path?q=1\n" {
			t.Fatalf("local proxy %s returned %d %q: %v", local.Host, response.StatusCode, body, err)
		}
		if svc.Worktree == feature {
			orphan = svc
		}
		if svc.Worktree == second {
			secondService = svc
		}
		if svc.Worktree == first {
			mainService = svc
		}
	}

	alias, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/alias", proxyPort), nil)
	if err != nil {
		t.Fatal(err)
	}
	alias.Host = "first.localhost"
	response, err := (&http.Client{Timeout: time.Second}).Do(alias)
	if err != nil {
		t.Fatal(err)
	}
	aliasBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(aliasBody) != "poc ready first "+first+" /alias\n" {
		t.Fatalf("project alias returned %d %q: %v", response.StatusCode, aliasBody, err)
	}
	unknown, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/", proxyPort), nil)
	if err != nil {
		t.Fatal(err)
	}
	unknown.Host = "unknown.localhost"
	response, err = (&http.Client{Timeout: time.Second}).Do(unknown)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown host returned %d, want 404", response.StatusCode)
	}
	if mainService.RunID == "" {
		t.Fatal("main worktree service was not found")
	}
	manifest, err := store.ReadManifest(mainService.RunID)
	if err != nil {
		t.Fatal(err)
	}
	writeNetwork := func(settings networking.Config) {
		t.Helper()
		data, err := json.Marshal(settings)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(configRoot, "networking.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	assertRun := func(wantRestart bool) {
		t.Helper()
		current, err := store.ReadManifest(mainService.RunID)
		if err != nil || !reflect.DeepEqual(current, manifest) {
			t.Fatalf("running launch manifest changed: %+v %v", current, err)
		}
		cmd := exec.CommandContext(ctx, binary, "status", "--json")
		cmd.Dir = first
		data, err := cmd.Output()
		if err != nil {
			t.Fatalf("status: %v %s", err, data)
		}
		var status struct {
			Results []struct {
				InstanceID      string `json:"instance_id"`
				RunID           string `json:"run_id"`
				RestartRequired bool   `json:"restart_required"`
			} `json:"results"`
		}
		if err := json.Unmarshal(data, &status); err != nil {
			t.Fatal(err)
		}
		for _, result := range status.Results {
			if result.InstanceID == mainService.ID {
				if result.RunID != mainService.RunID || result.RestartRequired != wantRestart {
					t.Fatalf("unexpected run status: %+v, want restart=%t", result, wantRestart)
				}
				return
			}
		}
		t.Fatalf("main service missing from status: %s", data)
	}
	settings := networking.DefaultConfig()
	settings.Local.Listen = fmt.Sprintf("127.0.0.1:%d", proxyPort)
	settings.Tailnet.Domain = "dev.example.com"
	settings.Projects[mainService.ProjectID] = networking.Project{Tailnet: true}
	writeNetwork(settings)
	invoke(first, true, "proxy", "reload")
	assertRun(false)
	settings.Projects[mainService.ProjectID] = networking.Project{}
	writeNetwork(settings)
	invoke(first, true, "proxy", "reload")
	assertRun(false)
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	settings.Local.Listen = occupied.Addr().String()
	writeNetwork(settings)
	conflict := exec.CommandContext(ctx, binary, "proxy", "reload", "--json")
	conflict.Dir = first
	conflictOutput, conflictErr := conflict.CombinedOutput()
	occupied.Close()
	if conflictErr == nil || !strings.Contains(string(conflictOutput), `"PORT_CONFLICT"`) {
		t.Fatalf("occupied listener was not reported: err=%v output=%s", conflictErr, conflictOutput)
	}
	response, err = (&http.Client{Timeout: time.Second}).Do(alias)
	if err != nil {
		t.Fatalf("port conflict broke existing listener: %v", err)
	}
	aliasBody, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(aliasBody) != "poc ready first "+first+" /alias\n" {
		t.Fatalf("port conflict broke existing route: %d %q: %v", response.StatusCode, aliasBody, err)
	}
	assertRun(false)
	newListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	newPort := newListener.Addr().(*net.TCPAddr).Port
	newListener.Close()
	settings.Local.Listen = fmt.Sprintf("127.0.0.1:%d", newPort)
	writeNetwork(settings)
	invoke(first, true, "proxy", "reload")
	assertRun(true)
	proxyPort = newPort
	alias.URL.Host = fmt.Sprintf("127.0.0.1:%d", proxyPort)
	response, err = (&http.Client{Timeout: time.Second}).Do(alias)
	if err != nil {
		t.Fatal(err)
	}
	aliasBody, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(aliasBody) != "poc ready first "+first+" /alias\n" {
		t.Fatalf("alias after listener change: %d %q: %v", response.StatusCode, aliasBody, err)
	}
	if orphan.ID == "" {
		t.Fatal("feature service not found")
	}
	scoped, err := monitor.Read(ctx, store, feature)
	if err != nil || scoped.CurrentProjectID != orphan.ProjectID {
		t.Fatalf("worktree project scope: %+v err=%v", scoped, err)
	}
	invoke(second, false, "up", "broken", "--no-proxy")
	failure, err := monitor.Read(ctx, store, second)
	if err != nil || !failure.PreferErrors {
		t.Fatalf("last failed launch: %+v err=%v", failure, err)
	}
	foundFailure := false
	for _, svc := range failure.Services {
		if svc.Name == "broken" && svc.Error != "" {
			foundFailure = true
			log, err := monitor.Logs(ctx, store, svc, 50)
			if err != nil || !strings.Contains(log, "poc build failure") {
				t.Fatalf("failed build log: %q err=%v", log, err)
			}
		}
	}
	if !foundFailure {
		t.Fatal("failed first launch missing from snapshot")
	}
	if err := os.RemoveAll(feature); err != nil {
		t.Fatal(err)
	}
	snapshot, err = monitor.Read(ctx, store, base)
	if err != nil {
		t.Fatal(err)
	}
	foundOrphan := false
	for _, svc := range snapshot.Services {
		if svc.ID == orphan.ID {
			foundOrphan = svc.MissingWorktree
			orphan = svc
		}
	}
	if !foundOrphan {
		t.Fatal("running process of deleted worktree disappeared")
	}
	owner, err := procid.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	publication := state.Publication{ID: "orphan-share", ProjectID: orphan.ProjectID, RootID: orphan.ID, RootRunID: orphan.RunID,
		Mode: "live", Phase: "preparing", Cloudflared: "/bin/true", Owner: owner,
		Salt: make([]byte, 16), Verifier: make([]byte, 32), SigningKey: make([]byte, 32)}
	if err := store.WritePublications(state.PublicationRegistry{Version: 1, Publications: map[string]state.Publication{publication.ID: publication}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(reg *state.Registry) error {
		p, err := reg.Project(orphan.ProjectID)
		if err != nil {
			return err
		}
		i := p.Instances[orphan.ID]
		i.Hook = &state.HookIntent{RunID: orphan.RunID, OperationID: "previous-stop", Name: "before_stop", Phase: "failed"}
		p.Instances[i.ID] = i
		return reg.SetProject(p.ID, p)
	}); err != nil {
		t.Fatal(err)
	}
	if err := monitor.Stop(ctx, store, orphan); err != nil {
		t.Fatalf("stop process without worktree: %v", err)
	}
	publications, err := store.ReadPublications()
	if err != nil || publications.Publications[publication.ID].Phase != "closed" {
		t.Fatalf("orphan stop left publication open: %+v err=%v", publications, err)
	}
	snapshot, err = monitor.Read(ctx, store, base)
	if err != nil {
		t.Fatal(err)
	}
	live := 0
	for _, svc := range snapshot.Services {
		if svc.ID == orphan.ID {
			t.Fatal("deleted worktree still visible after stop")
		}
		if svc.Name == "web" && svc.Error == "" {
			live++
		}
	}
	if live != 2 {
		t.Fatalf("stopping orphan affected unrelated services: %+v", snapshot.Services)
	}
	if secondService.ID == "" {
		t.Fatal("second project's service was not found")
	}
	invoke(second, true, "down", "web")
	stoppedURL, err := url.Parse(secondService.LocalURL)
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/", proxyPort), nil)
	if err != nil {
		t.Fatal(err)
	}
	stopped.Host = stoppedURL.Host
	response, err = (&http.Client{Timeout: time.Second}).Do(stopped)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("stopped known service returned %d, want 503", response.StatusCode)
	}

	clone := filepath.Join(base, "clone", "first")
	if err := os.MkdirAll(clone, 0700); err != nil {
		t.Fatal(err)
	}
	writeConfig(clone, "first", firstPort)
	invoke(clone, false, "up", "web")
	response, err = (&http.Client{Timeout: time.Second}).Do(alias)
	if err != nil {
		t.Fatal(err)
	}
	aliasBody, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(aliasBody) != "poc ready first "+first+" /alias\n" {
		t.Fatalf("clone collision changed existing route: status=%d body=%q err=%v", response.StatusCode, aliasBody, err)
	}

	invoke(first, true, "proxy", "stop")
	blocker, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", proxyPort))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	start := exec.CommandContext(ctx, binary, "up", "web", "--json")
	start.Dir = first
	startOutput, startErr := start.CombinedOutput()
	if startErr == nil || !strings.Contains(string(startOutput), `"PORT_CONFLICT"`) {
		t.Fatalf("startup port conflict not reported: err=%v output=%s", startErr, startOutput)
	}
	response, err = (&http.Client{Timeout: time.Second}).Get(mainService.DirectURL)
	if err != nil {
		t.Fatalf("startup port conflict stopped live app: %v", err)
	}
	response.Body.Close()
	assertRun(true)
}

func TestLocalLaunchURLs(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs real rdr service processes")
	}
	base, err := os.MkdirTemp("", "rdr-launch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	_, source, _, _ := runtime.Caller(0)
	repo := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	binary := filepath.Join(base, "rdr")
	build := exec.Command("go", "build", "-o", binary, "./cmd/rdr")
	build.Dir = repo
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build rdr: %v\n%s", err, out)
	}
	project, configRoot := filepath.Join(base, "project"), filepath.Join(base, "config", "rundevrun")
	for _, dir := range []string{project, configRoot} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("RDR_HOME", filepath.Join(base, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
	store := &state.Store{Root: os.Getenv("RDR_HOME")}
	freePort := func() int {
		t.Helper()
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	settings := networking.DefaultConfig()
	proxyPortLock, oldPort := reservePort(t)
	apiPortLock, apiPort := reservePort(t)
	webPortLock, webPort := reservePort(t)
	settings.Local.Listen = fmt.Sprintf("127.0.0.1:%d", oldPort)
	writeSettings := func() {
		t.Helper()
		b, err := json.Marshal(settings)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(configRoot, "networking.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeSettings()
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := "'" + strings.ReplaceAll(helper, "'", "'\\''") + "' -test.run=^TestLaunchURLHelper$"
	config := fmt.Sprintf(`version = 1
[project]
name = "launch"
[services.api]
command = %s
port = %d
protocol = "http"
health = { http = "/" }
env = { POC_LAUNCH_TEST = "1", MARKER = "api.json", OWN_VALUE = "{url}", REPORT_MODE = "owned" }
[services.web]
command = %s
port = %d
protocol = "http"
health = { http = "/" }
depends_on = ["api"]
env = { POC_LAUNCH_TEST = "1", MARKER = "web.json", OWN_VALUE = "{url}", DEP_VALUE = "{dependencies.api.url}", REPORT_MODE = "external" }
`, strconv.Quote(command), apiPort, strconv.Quote(command), webPort)
	if err := os.WriteFile(filepath.Join(project, "dev.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) {
		t.Helper()
		cmd := exec.Command(binary, append(args, "--json")...)
		cmd.Dir = project
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("rdr %v: %v %s", args, err, out)
		}
	}
	t.Cleanup(func() {
		for _, args := range [][]string{{"down", "--all", "--json"}, {"proxy", "stop", "--json"}} {
			cmd := exec.Command(binary, args...)
			cmd.Dir = project
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Logf("cleanup %v: %v %s", args, err, out)
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	read := func() (map[string]monitor.Service, map[string]state.Manifest, map[string]map[string]string) {
		t.Helper()
		snap, err := monitor.Read(ctx, store, project)
		if err != nil {
			t.Fatal(err)
		}
		services, manifests, envs := map[string]monitor.Service{}, map[string]state.Manifest{}, map[string]map[string]string{}
		for _, svc := range snap.Services {
			services[svc.Name] = svc
			manifest, err := store.ReadManifest(svc.RunID)
			if err != nil {
				t.Fatal(err)
			}
			manifests[svc.Name] = manifest
			b, err := os.ReadFile(filepath.Join(project, svc.Name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var childEnv map[string]string
			if err := json.Unmarshal(b, &childEnv); err != nil {
				t.Fatal(err)
			}
			envs[svc.Name] = childEnv
		}
		return services, manifests, envs
	}
	assertURLs := func(manifests map[string]state.Manifest, envs map[string]map[string]string, port int) {
		t.Helper()
		api := fmt.Sprintf("http://api-project-launch.localhost:%d", port)
		web := fmt.Sprintf("http://web-project-launch.localhost:%d", port)
		if manifests["api"].Addresses["url"] != api || envs["api"]["RDR_URL"] != api || envs["api"]["OWN_VALUE"] != api {
			t.Fatalf("api launch URLs: manifest=%+v env=%+v want=%s", manifests["api"].Addresses, envs["api"], api)
		}
		if manifests["web"].Addresses["url"] != web || envs["web"]["RDR_URL"] != web || envs["web"]["OWN_VALUE"] != web ||
			envs["web"]["RDR_DEP_API_URL"] != api || envs["web"]["DEP_VALUE"] != api {
			t.Fatalf("web launch URLs: manifest=%+v env=%+v want own=%s dependency=%s", manifests["web"].Addresses, envs["web"], web, api)
		}
	}
	assertRestart := func(want bool) {
		t.Helper()
		cmd := exec.Command(binary, "status", "--json")
		cmd.Dir = project
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("status: %v %s", err, out)
		}
		var status struct {
			Results []struct {
				Service         string `json:"service"`
				RestartRequired bool   `json:"restart_required"`
			} `json:"results"`
		}
		if err := json.Unmarshal(out, &status); err != nil {
			t.Fatal(err)
		}
		if len(status.Results) != 2 {
			t.Fatalf("missing status results: %s", out)
		}
		for _, item := range status.Results {
			if item.RestartRequired != want {
				t.Fatalf("%s restart_required=%t, want %t", item.Service, item.RestartRequired, want)
			}
		}
	}
	proxyPortLock.Close()
	apiPortLock.Close()
	webPortLock.Close()
	invoke("up", "web")
	services, manifests, envs := read()
	if len(services) != 2 {
		t.Fatalf("expected api and web: %+v", services)
	}
	assertURLs(manifests, envs, oldPort)
	assertRestart(false)
	openURL := func(service string) string {
		t.Helper()
		cmd := exec.Command(binary, "open", service, "--json")
		cmd.Dir = project
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("open %s: %v %s", service, err, out)
		}
		var result struct {
			Results []struct {
				URL string `json:"url"`
			} `json:"results"`
		}
		if err := json.Unmarshal(out, &result); err != nil || len(result.Results) != 1 {
			t.Fatalf("open %s result: %s (%v)", service, out, err)
		}
		return result.Results[0].URL
	}
	wantOwned := fmt.Sprintf("http://api-project-launch.localhost:%d/s/session/?x=1#frag", oldPort)
	if got := openURL("api"); got != wantOwned {
		t.Fatalf("owned announcement: got %q, want %q", got, wantOwned)
	}
	wantWeb := fmt.Sprintf("http://web-project-launch.localhost:%d", oldPort)
	if got := openURL("web"); got != wantWeb {
		t.Fatalf("external announcement rewrote local URL: got %q, want %q", got, wantWeb)
	}
	if services["web"].ReportedURL != "https://outside.example/s/external/?token=public#external" {
		t.Fatalf("external announcement was lost: %+v", services["web"])
	}
	settings.Tailnet.Domain = "dev.example.com"
	settings.Projects[services["api"].ProjectID] = networking.Project{Tailnet: true}
	writeSettings()
	invoke("proxy", "reload")
	assertRestart(false)
	textOutput := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Dir = project
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("rdr %v: %v %s", args, err, out)
		}
		if strings.Contains(string(out), "\x1b[") {
			t.Fatalf("rdr %v output uses color when piped: %q", args, out)
		}
		return string(out)
	}
	const connectedURL = "https://api-project-launch.dev.example.com:443/s/session/?x=1#frag"
	for _, tc := range []struct{ command, symbol string }{{"up", "●"}, {"restart", "✓"}} {
		wantTable := "  SERVICE  URL\n" + tc.symbol + " api      " + wantOwned + "\n           " + connectedURL + "\n"
		if got := textOutput(tc.command, "api"); !strings.HasPrefix(got, wantTable) {
			t.Fatalf("rdr %s api output = %q, want prefix %q", tc.command, got, wantTable)
		}
	}
	services, manifests, envs = read()
	settings.Projects[services["api"].ProjectID] = networking.Project{}
	writeSettings()
	invoke("proxy", "reload")
	assertRestart(false)
	newPort := freePort()
	settings.Local.Listen = fmt.Sprintf("127.0.0.1:%d", newPort)
	writeSettings()
	invoke("proxy", "reload")
	assertRestart(true)
	wantOwned = fmt.Sprintf("http://api-project-launch.localhost:%d/s/session/?x=1#frag", newPort)
	if got := openURL("api"); got != wantOwned {
		t.Fatalf("owned announcement after route change: got %q, want %q", got, wantOwned)
	}
	wantWeb = fmt.Sprintf("http://web-project-launch.localhost:%d", newPort)
	if got := openURL("web"); got != wantWeb {
		t.Fatalf("external announcement after route change: got %q, want %q", got, wantWeb)
	}
	afterServices, afterManifests, afterEnvs := read()
	for _, name := range []string{"api", "web"} {
		if afterServices[name].RunID != services[name].RunID || !reflect.DeepEqual(afterManifests[name], manifests[name]) || !reflect.DeepEqual(afterEnvs[name], envs[name]) {
			t.Fatalf("%s launch changed on route update: before=%+v/%+v after=%+v/%+v", name, manifests[name], envs[name], afterManifests[name], afterEnvs[name])
		}
	}
	invoke("restart", "api")
	invoke("restart", "web")
	restarted, newManifests, newEnvs := read()
	for _, name := range []string{"api", "web"} {
		if restarted[name].RunID == services[name].RunID {
			t.Fatalf("%s did not get a new RunID after restart", name)
		}
	}
	assertURLs(newManifests, newEnvs, newPort)
	assertRestart(false)
	if got := openURL("api"); got != wantOwned {
		t.Fatalf("owned announcement after restart: got %q, want %q", got, wantOwned)
	}
	if got, want := textOutput("down", "web", "api"), "  SERVICE\n✓ web\n✓ api\n"; got != want {
		t.Fatalf("rdr down web api output = %q, want %q", got, want)
	}
	if got, want := textOutput("down", "api"), "  SERVICE\n● api\n"; got != want {
		t.Fatalf("rdr down api output = %q, want %q", got, want)
	}
}
