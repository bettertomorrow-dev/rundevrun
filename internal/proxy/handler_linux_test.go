//go:build linux

package proxy

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestProxyHelper(t *testing.T) {
	if os.Getenv("RDR_PROXY_HELPER") != "yes" {
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(2)
	}
	fmt.Println(ln.Addr().(*net.TCPAddr).Port)
	_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			conn, rw, e := w.(http.Hijacker).Hijack()
			if e != nil {
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
			_ = rw.Flush()
			_, _ = io.Copy(conn, conn)
			return
		}
		if r.URL.Path == "/sse" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(time.Second)
			_, _ = io.WriteString(w, "data: second\n\n")
			return
		}
		w.Header().Add("Set-Cookie", "app_session=fixture; Secure; HttpOnly; SameSite=Lax")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"path": r.URL.Path, "host": r.Host, "proto": r.Header.Get("X-Forwarded-Proto"),
			"xff": r.Header.Get("X-Forwarded-For"), "forwarded": r.Header.Get("Forwarded"),
			"port": r.Header.Get("X-Forwarded-Port"), "prefix": r.Header.Get("X-Forwarded-Prefix"),
			"real_ip": r.Header.Get("X-Real-IP"), "cookie": r.Header.Get("Cookie"),
			"pid": strconv.Itoa(os.Getpid()),
		})
	}))
	os.Exit(0)
}

func fixture(t *testing.T) (*state.Store, routing.Config, string) {
	t.Helper()
	start := func() (int, procid.Ref) {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^TestProxyHelper$")
		cmd.Env = append(os.Environ(), "RDR_PROXY_HELPER=yes")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		port, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			t.Fatal(err)
		}
		ref, err := procid.Observe(cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		return port, ref
	}
	port, ref := start()
	depPort, depRef := start()
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := "p"
	src := "p/w/web"
	dep := "p/w/api"
	inst := state.Instance{ID: src, Service: "web", WorktreeID: "w", Hostname: "web.example.com", State: "starting", RunID: "web-run"}
	target := state.Instance{ID: dep, Service: "api", WorktreeID: "w", State: "starting", RunID: "api-run"}
	err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject(project, state.Project{ID: project, Name: "p", Root: "/tmp/p", Worktrees: []state.Worktree{{ID: "w", Root: "/tmp/p"}}, Instances: map[string]state.Instance{src: inst, dep: target}})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []state.Manifest{{RunID: "web-run", InstanceID: src, Launch: state.LaunchContract{Protocol: "http", Port: port}, Routes: []state.Route{{Path: "/api", Dependency: "api", StripPrefix: true}}, EffectiveBindings: map[string]string{"api": dep}}, {RunID: "api-run", InstanceID: dep, Launch: state.LaunchContract{Protocol: "http", Port: depPort}}} {
		if err = store.WriteManifest(m.RunID, m); err != nil {
			t.Fatal(err)
		}
		targetRef := ref
		if m.RunID == "api-run" {
			targetRef = depRef
		}
		if err = store.WriteRun(m.RunID, state.RunRecord{Claim: "accepted", Supervisor: targetRef, Child: targetRef, State: "running"}); err != nil {
			t.Fatal(err)
		}
	}
	return store, routing.Config{}, src
}
func TestHandler(t *testing.T) {
	store, cfg, src := fixture(t)
	h := NewHandler(store, cfg)
	req := httptest.NewRequest("GET", "https://web.example.com/api/hello", nil)
	req.RemoteAddr = "192.0.2.2:1234"
	req.TLS = &tls.ConnectionState{ServerName: "web.example.com"}
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("Forwarded", "for=evil")
	req.Header.Set("X-Forwarded-For", "8.8.8.8")
	req.Header.Set("X-Forwarded-Port", "1234")
	req.Header.Set("X-Forwarded-Prefix", "/spoof")
	req.Header.Set("X-Real-IP", "8.8.8.8")
	req.Header.Set("Cookie", "project_cookie=keep")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("proxy: %d %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	apiPID := body["pid"]
	rootReq := httptest.NewRequest("GET", "https://web.example.com/", nil)
	rootReq.TLS = &tls.ConnectionState{ServerName: "web.example.com"}
	rootResp := httptest.NewRecorder()
	h.ServeHTTP(rootResp, rootReq)
	var rootBody map[string]string
	if rootResp.Code != 200 || json.Unmarshal(rootResp.Body.Bytes(), &rootBody) != nil || apiPID == "" || rootBody["pid"] == apiPID {
		t.Fatalf("dependency route did not select its bound process: %s", rootResp.Body.String())
	}
	if body["path"] != "/hello" || body["host"] != "web.example.com" || body["proto"] != "https" || body["xff"] != "192.0.2.2" || body["forwarded"] != "" {
		t.Fatalf("headers: %#v", body)
	}
	if body["port"] != "" || body["prefix"] != "" || body["real_ip"] != "" || body["cookie"] != "project_cookie=keep" || w.Header().Get("Set-Cookie") != "app_session=fixture; Secure; HttpOnly; SameSite=Lax" {
		t.Fatalf("forwarded headers or application cookies changed: %#v, %#v", body, w.Header())
	}
	req = httptest.NewRequest("GET", "https://unknown.example.com/", nil)
	req.TLS = &tls.ConnectionState{ServerName: "unknown.example.com"}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("unknown: %d", w.Code)
	}
	req = httptest.NewRequest("GET", "https://web.example.com/", nil)
	req.TLS = &tls.ConnectionState{ServerName: "other.example.com"}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 421 {
		t.Fatalf("fronting: %d", w.Code)
	}
	trustedCfg := cfg
	trustedCfg.Routing.TrustedProxies = []string{"127.0.0.1"}
	trustedReq := httptest.NewRequest("GET", "http://proxy.local/", nil)
	trustedReq.RemoteAddr = "127.0.0.1:1234"
	trustedReq.Header.Set("X-Forwarded-Host", "web.example.com")
	trustedReq.Header.Set("X-Forwarded-Proto", "https")
	trustedReq.Header.Set("X-Forwarded-For", "192.0.2.9")
	trustedResp := httptest.NewRecorder()
	NewHandler(store, trustedCfg).ServeHTTP(trustedResp, trustedReq)
	var trustedBody map[string]string
	if trustedResp.Code != 200 || json.Unmarshal(trustedResp.Body.Bytes(), &trustedBody) != nil || trustedBody["proto"] != "https" || trustedBody["host"] != "web.example.com" || trustedBody["xff"] != "192.0.2.9, 127.0.0.1" {
		t.Fatalf("trusted forwarding: %d %s", trustedResp.Code, trustedResp.Body.String())
	}
	starting, err := store.ReadRun("web-run")
	if err != nil {
		t.Fatal(err)
	}
	pending := starting
	pending.State = "starting"
	if err = store.WriteRun("web-run", pending); err != nil {
		t.Fatal(err)
	}
	gateReq := httptest.NewRequest("GET", "https://web.example.com/", nil)
	gateReq.TLS = &tls.ConnectionState{ServerName: "web.example.com"}
	gateResp := httptest.NewRecorder()
	h.ServeHTTP(gateResp, gateReq)
	if gateResp.Code != 503 {
		t.Fatalf("startup gate: %d", gateResp.Code)
	}
	if err = store.WriteRun("web-run", starting); err != nil {
		t.Fatal(err)
	}
	// An unverified child must never be replaced by a listener on the same port.
	original, err := store.ReadRun("api-run")
	if err != nil {
		t.Fatal(err)
	}
	stale := original
	stale.Child.Identity = "foreign"
	if err = store.WriteRun("api-run", stale); err != nil {
		t.Fatal(err)
	}
	staleReq := httptest.NewRequest("GET", "https://web.example.com/api/hello", nil)
	staleReq.TLS = &tls.ConnectionState{ServerName: "web.example.com"}
	staleResp := httptest.NewRecorder()
	h.ServeHTTP(staleResp, staleReq)
	if staleResp.Code != 503 {
		t.Fatalf("stale target: %d", staleResp.Code)
	}
	if err = store.WriteRun("api-run", original); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(reg *state.Registry) error {
		p, e := reg.Project("p")
		if e != nil {
			return e
		}
		inst := p.Instances[src]
		inst.State = "stopped"
		p.Instances[src] = inst
		return reg.SetProject("p", p)
	}); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("GET", "https://web.example.com/", nil)
	req.TLS = &tls.ConnectionState{ServerName: "web.example.com"}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 503 {
		t.Fatalf("stopped: %d", w.Code)
	}
}
func TestStreaming(t *testing.T) {
	store, cfg, _ := fixture(t)
	server := httptest.NewServer(NewHandler(store, cfg))
	defer server.Close()
	req, _ := http.NewRequest("GET", server.URL+"/sse", nil)
	req.Host = "web.example.com"
	start := time.Now()
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	first, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || first != "data: first\n" || time.Since(start) > 750*time.Millisecond {
		t.Fatalf("first SSE=%q elapsed=%v err=%v", first, time.Since(start), err)
	}
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprint(conn, "GET /socket HTTP/1.1\r\nHost: web.example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	br := bufio.NewReader(conn)
	response, err = http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil || response.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", response, err)
	}
	_, _ = io.WriteString(conn, "hello")
	reply := make([]byte, 5)
	if _, err = io.ReadFull(br, reply); err != nil || string(reply) != "hello" {
		t.Fatalf("websocket %q %v", reply, err)
	}
}

func TestTLSHost(t *testing.T) {
	store, cfg, _ := fixture(t)
	server := httptest.NewTLSServer(NewHandler(store, cfg))
	defer server.Close()
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: "web.example.com"}} // ephemeral test certificate
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, _ := http.NewRequest("GET", server.URL+"/", nil)
	request.Host = "web.example.com"
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("TLS: %d", response.StatusCode)
	}
	request, _ = http.NewRequest("GET", server.URL+"/", nil)
	request.Host = "other.example.com"
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 421 {
		t.Fatalf("fronting: %d", response.StatusCode)
	}
}

func TestPinnedPublicationRoutesAndRunIdentity(t *testing.T) {
	store, _, _ := fixture(t)
	pins := map[string]string{"p/w/api": "api-run"}
	h := NewPinnedHandler(store, "p", "p/w/web", "web-run", pins)
	req := httptest.NewRequest("GET", "http://gateway/api/hello", nil)
	req.Host = "public.trycloudflare.com"
	req.Header.Set("Cookie", "application=keep")
	req.Header.Set("X-Forwarded-Proto", "http")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("routed: %d %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if e := json.Unmarshal(w.Body.Bytes(), &body); e != nil {
		t.Fatal(e)
	}
	if body["path"] != "/hello" || body["host"] != "public.trycloudflare.com" || body["proto"] != "https" || body["cookie"] != "application=keep" || w.Header().Get("Set-Cookie") == "" {
		t.Fatalf("forwarding: %+v", body)
	}
	// A changed routed run cannot reach the still-listening old process.
	if e := store.Update(func(reg *state.Registry) error {
		p, e := reg.Project("p")
		if e != nil {
			return e
		}
		i := p.Instances["p/w/api"]
		i.RunID = "different-run"
		p.Instances[i.ID] = i
		return reg.SetProject(p.ID, p)
	}); e != nil {
		t.Fatal(e)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("changed target accepted: %d", w.Code)
	}
	w = httptest.NewRecorder()
	NewPinnedHandler(store, "p", "p/w/web", "wrong-run", pins).ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("invalid root accepted: %d", w.Code)
	}
}
func TestPinnedPublicationRootStripKeepsAbsolutePath(t *testing.T) {
	store, _, _ := fixture(t)
	path := filepath.Join(store.Root, "runs", "web-run", "manifest.json")
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var m state.Manifest
	if e = json.Unmarshal(raw, &m); e != nil {
		t.Fatal(e)
	}
	m.Routes = []state.Route{{Path: "/", Dependency: "api", StripPrefix: true}}
	raw, e = json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	h := NewPinnedHandler(store, "p", "p/w/web", "web-run", map[string]string{"p/w/api": "api-run"})
	req := httptest.NewRequest("GET", "http://gateway/identity", nil)
	req.Host = "public.trycloudflare.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var body map[string]string
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["path"] != "/identity" {
		t.Fatalf("root strip: %d %s", w.Code, w.Body.String())
	}
}
