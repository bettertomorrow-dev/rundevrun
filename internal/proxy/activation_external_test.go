// Tests proxy activation external behavior.
package proxy_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/networkactivation"
	"github.com/bettertomorrow-dev/rundevrun/internal/networking"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/proxy"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestActivationTargetHelper(t *testing.T) {
	if os.Getenv("RDR_ACTIVATION_TARGET") != "1" {
		return
	}
	if os.Getenv("RDR_ACTIVATION_SUPERVISOR") == "1" {
		exe, err := os.Executable()
		if err != nil {
			os.Exit(2)
		}
		cmd := exec.Command(exe, "-test.run=^TestActivationTargetHelper$")
		cmd.Env = append(os.Environ(), "RDR_ACTIVATION_SUPERVISOR=0")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		os.Exit(2)
	}
	fmt.Println(listener.Addr().(*net.TCPAddr).Port, os.Getpid())
	_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "owned activation target") }))
	os.Exit(0)
}

func TestActivationEnablesRealManagedHTTPS(t *testing.T) {

	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	localAddress := local.Addr().String()
	local.Close()
	root, err := os.MkdirTemp("/tmp", "rdr-activate-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	configRoot := filepath.Join(root, "config")
	if err = os.Mkdir(configRoot, 0700); err != nil {
		t.Fatal(err)
	}
	desired := networking.DefaultConfig()
	desired.Local.Listen = localAddress
	desired.Tailnet.Domain = "tail.example.com"
	desired.Projects["p"] = networking.Project{Tailnet: true}
	data, _ := json.Marshal(desired)
	if err = os.WriteFile(filepath.Join(configRoot, "networking.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestActivationTargetHelper$")
	cmd.Env = append(os.Environ(), "RDR_ACTIVATION_TARGET=1", "RDR_ACTIVATION_SUPERVISOR=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var port, pid int
	_, err = fmt.Sscan(line, &port, &pid)
	if err != nil {
		t.Fatal(err)
	}
	child, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Kill()
	ref, err := procid.Observe(pid)
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := procid.Observe(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.New(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	id := "p/w/web"
	instance := state.Instance{ID: id, Service: "web", WorktreeID: "w", State: "running", RunID: "activation-run", Endpoints: []state.Endpoint{{Scope: "tailnet", Hostname: "web.tail.example.com", Port: 443, InstanceID: id}}}
	if err = store.Update(func(reg *state.Registry) error {
		return reg.SetProject("p", state.Project{ID: "p", Name: "demo", Root: filepath.Join(root, "deleted"), Worktrees: []state.Worktree{{ID: "w", Root: filepath.Join(root, "deleted")}}, Instances: map[string]state.Instance{id: instance}})
	}); err != nil {
		t.Fatal(err)
	}
	if err = store.WriteManifest(instance.RunID, state.Manifest{RunID: instance.RunID, InstanceID: id, Launch: state.LaunchContract{Protocol: "http", Port: port}}); err != nil {
		t.Fatal(err)
	}
	if err = store.WriteRun(instance.RunID, state.RunRecord{Claim: "accepted", Supervisor: supervisor, Child: ref, State: "running"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.VerifyProxyTarget(store, "p", id, instance.RunID); err != nil {
		t.Fatalf("activation target ownership: %v", err)
	}
	tcp, err := net.Listen("tcp", "127.0.0.1:8443")
	if err != nil {
		t.Skipf("managed test port unavailable: %v", err)
	}
	tcp.Close()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(42), DNSNames: []string{"*.tail.example.com"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, _ := x509.MarshalPKCS8PrivateKey(key)
	pair, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))
	if err != nil {
		t.Fatal(err)
	}
	pair.Leaf, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.RWMutex
	published := false
	restore := proxy.SetCertificateLoaderForTest(func(networking.Service, string) (*tls.Certificate, error) {
		mu.RLock()
		defer mu.RUnlock()
		if !published {
			return nil, errors.New("certificate not yet issued")
		}
		return &pair, nil
	})
	defer restore()
	configPath := filepath.Join(configRoot, "config.toml")
	done := make(chan error, 1)
	go func() { done <- proxy.Serve(store.Root, configPath) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, e := proxy.Status(store)
		if e == nil && info.State == "ready" {
			if info.TLSMode != "local" || info.HTTPSListen != "" {
				t.Fatalf("unexpected initial status: %+v", info)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy did not start: %v", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = proxy.StopInProcessForTest(ctx, store)
		select {
		case e := <-done:
			if e != nil {
				t.Errorf("proxy: %v", e)
			}
		case <-ctx.Done():
			t.Error("proxy did not stop")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	failed, err := networkactivation.Prepare(ctx, store, exe, configPath, desired)
	if err != nil {
		t.Fatal(err)
	}
	if err = failed.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = failed.Finish(ctx); err == nil {
		t.Fatal("activation succeeded without a certificate")
	}
	initial, err := proxy.Status(store)
	if err != nil || initial.PreparedListen != "" || initial.TLSMode != "local" {
		t.Fatalf("failed activation leaked reservation: %+v %v", initial, err)
	}
	activation, err := networkactivation.Prepare(ctx, store, exe, configPath, desired)
	if err != nil {
		t.Fatal(err)
	}
	defer activation.Abort()
	info, err := proxy.Status(store)
	if err != nil || info.PreparedListen != localAddress {
		t.Fatalf("reservation missing: %+v %v", info, err)
	}

	mu.Lock()
	published = true
	mu.Unlock()
	if err = activation.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = activation.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	info, err = proxy.Status(store)
	if err != nil || info.TLSMode != "managed" || info.HTTPSListen != "127.0.0.1:8443" || info.PreparedListen != "" {
		t.Fatalf("activation status: %+v %v", info, err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(pair.Leaf)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "web.tail.example.com"}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", "127.0.0.1:8443")
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Get("https://web.tail.example.com/path")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || string(body) != "owned activation target" {
		t.Fatalf("HTTPS status=%d body=%q err=%v", response.StatusCode, body, err)
	}
}
