//go:build linux

package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == "__rdr_proxy" {
		if Serve(os.Args[2], os.Args[3]) != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 4 && os.Args[1] == "__rdr_proxy_parent" {
		cmd := exec.Command(os.Args[0], "__rdr_proxy", os.Args[2], os.Args[3])
		if err := cmd.Start(); err != nil {
			os.Exit(1)
		}
		_ = cmd.Process.Release()
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func freeAddress(t *testing.T) string {
	t.Helper()
	l, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	a := l.Addr().String()
	_ = l.Close()
	return a
}
func certPair(t *testing.T, dir string, serial int64) (string, string) {
	t.Helper()
	k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{"web.example.com"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	cert, e := x509.CreateCertificate(rand.Reader, template, template, &k.PublicKey, k)
	if e != nil {
		t.Fatal(e)
	}
	key, e := x509.MarshalPKCS8PrivateKey(k)
	if e != nil {
		t.Fatal(e)
	}
	certPath := filepath.Join(dir, fmt.Sprintf("cert%d.pem", serial))
	keyPath := filepath.Join(dir, fmt.Sprintf("key%d.pem", serial))
	if e = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); e != nil {
		t.Fatal(e)
	}
	return certPath, keyPath
}
func configFile(t *testing.T, dir, httpAddr, httpsAddr, cert, key string) string {
	t.Helper()
	path := filepath.Join(dir, "config.toml")
	text := fmt.Sprintf("version = 1\n[routing]\nenabled = true\ndomain = \"example.com\"\nhttp_listen = %q\nhttps_listen = %q\n[[routing.tls.certificates]]\ncert_file = %q\nkey_file = %q\n", httpAddr, httpsAddr, cert, key)
	if e := os.WriteFile(path, []byte(text), 0600); e != nil {
		t.Fatal(e)
	}
	return path
}
func proxyCert(t *testing.T, addr string) int64 {
	t.Helper()
	c, e := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, ServerName: "web.example.com"})
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	return c.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
}
func waitProxyStop(t *testing.T, store *state.Store) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		info, e := Status(store)
		if e == nil && info.State == "stopped" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("proxy did not stop")
}
func TestDaemonLifecycle(t *testing.T) {
	store, _, _ := fixture(t)
	dir := t.TempDir()
	a, b := freeAddress(t), freeAddress(t)
	cert, key := certPair(t, dir, 1)
	path := configFile(t, dir, a, b, cert, key)
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = Stop(context.Background(), store); waitProxyStop(t, store) })
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- Ensure(ctx, store, exe, path) }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	info, e := Status(store)
	if e != nil || info.State != "ready" {
		t.Fatalf("status: %+v %v", info, e)
	}
	if got := proxyCert(t, b); got != 1 {
		t.Fatalf("initial cert %d", got)
	}
	nextCert, nextKey := certPair(t, dir, 2)
	_ = configFile(t, dir, a, b, nextCert, nextKey)
	if e = Reload(ctx, store); e != nil {
		t.Fatal(e)
	}
	if got := proxyCert(t, b); got != 2 {
		t.Fatalf("reloaded cert %d", got)
	}
	_ = configFile(t, dir, a, b, nextCert, key)
	e = Reload(ctx, store)
	var controlErr *Error
	if !errors.As(e, &controlErr) || controlErr.Code != "TLS_INVALID" {
		t.Fatalf("invalid reload: %v", e)
	}
	if got := proxyCert(t, b); got != 2 {
		t.Fatalf("failed reload replaced certificate: %d", got)
	}
	if e = Ensure(ctx, store, exe, path); !errors.As(e, &controlErr) || controlErr.Code != "TLS_INVALID" {
		t.Fatalf("ensure discarded reload error: %v", e)
	}
	_ = configFile(t, dir, freeAddress(t), b, nextCert, nextKey)
	if e = Reload(ctx, store); !errors.As(e, &controlErr) || controlErr.Code != "CONFIG_INVALID" {
		t.Fatalf("listener change accepted: %v", e)
	}
	if got := proxyCert(t, b); got != 2 {
		t.Fatalf("address reload changed active config: %d", got)
	}

	req, e := http.NewRequest("GET", "http://"+a+"/", nil)
	if e != nil {
		t.Fatal(e)
	}
	req.Host = "web.example.com"
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP route %d", resp.StatusCode)
	}
	record, e := store.ReadRun("web-run")
	if e != nil {
		t.Fatal(e)
	}
	if e = Stop(ctx, store); e != nil {
		t.Fatal(e)
	}
	waitProxyStop(t, store)
	deadline := time.Now().Add(time.Second)
	for {
		_, statErr := os.Stat(fmt.Sprintf("/proc/%d", info.Identity.PID))
		if errors.Is(statErr, os.ErrNotExist) {
			break
		}
		if statErr != nil {
			t.Fatal(statErr)
		}
		current, observeErr := procid.Observe(info.Identity.PID)
		if observeErr == nil && current != info.Identity {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stopped proxy child was not reaped")
		}
		time.Sleep(5 * time.Millisecond)
	}
	alive, e := procid.Check(record.Child)
	if e != nil || !alive {
		t.Fatalf("app stopped with proxy: %v %v", alive, e)
	}
}
func TestDaemonParentExitAndForeignIdentity(t *testing.T) {
	store, _, _ := fixture(t)
	dir := t.TempDir()
	a, b := freeAddress(t), freeAddress(t)
	cert, key := certPair(t, dir, 3)
	path := configFile(t, dir, a, b, cert, key)
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = Stop(context.Background(), store); waitProxyStop(t, store) })
	parent := exec.Command(exe, "__rdr_proxy_parent", store.Root, path)
	if e = parent.Run(); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(8 * time.Second)
	var info Info
	for time.Now().Before(deadline) {
		info, e = Status(store)
		if e == nil && info.State == "ready" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if info.State != "ready" {
		t.Fatalf("orphan daemon not found: %+v %v", info, e)
	}
	if e = Stop(context.Background(), store); e != nil {
		t.Fatal(e)
	}
	waitProxyStop(t, store)
	self, e := procid.Observe(os.Getpid())
	if e != nil {
		t.Fatal(e)
	}
	if e = store.WriteProxy(state.ProxyRecord{Identity: self, State: "ready", ConfigPath: path}); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	e = Ensure(ctx, store, exe, path)
	var ce *Error
	if !errors.As(e, &ce) || ce.Code != "BUSY" {
		t.Fatalf("foreign identity: %v", e)
	}
	live, e := procid.Check(self)
	if e != nil || !live {
		t.Fatalf("foreign process affected: %v %v", live, e)
	}
	// Leave no live foreign record for cleanup to contact.
	if e = os.Remove(filepath.Join(store.Root, "proxy.json")); e != nil {
		t.Fatal(e)
	}
}

func TestTLSInvalidDoesNotStartListeners(t *testing.T) {
	store, _, _ := fixture(t)
	dir := t.TempDir()
	a, b := freeAddress(t), freeAddress(t)
	cert, _ := certPair(t, dir, 11)
	_, wrongKey := certPair(t, dir, 12)
	path := configFile(t, dir, a, b, cert, wrongKey)
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e = Ensure(ctx, store, exe, path)
	var ce *Error
	if !errors.As(e, &ce) || ce.Code != "TLS_INVALID" {
		t.Fatalf("invalid startup: %v", e)
	}
	for _, address := range []string{a, b} {
		ln, e := net.Listen("tcp", address)
		if e != nil {
			t.Fatalf("failed TLS left listener open at %s: %v", address, e)
		}
		_ = ln.Close()
	}
}
func TestStatusMissingIsReadOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	store := &state.Store{Root: root}
	info, e := Status(store)
	if e != nil || info.State != "stopped" {
		t.Fatalf("status: %+v %v", info, e)
	}
	if _, e = os.Stat(root); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("status created state root: %v", e)
	}
}

func TestUnsafeControlFilePreserved(t *testing.T) {
	store, _, _ := fixture(t)
	dir := t.TempDir()
	a, b := freeAddress(t), freeAddress(t)
	cert, key := certPair(t, dir, 31)
	path := configFile(t, dir, a, b, cert, key)
	socket := store.ProxySocket()
	if err := os.WriteFile(socket, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = Ensure(ctx, store, exe, path); err == nil {
		t.Fatal("accepted regular control file")
	}
	data, err := os.ReadFile(socket)
	if err != nil || string(data) != "preserve" {
		t.Fatalf("control file changed: %q %v", data, err)
	}
}
func TestPortConflictIsTyped(t *testing.T) {
	store, _, _ := fixture(t)
	dir := t.TempDir()
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	a, b := occupied.Addr().String(), freeAddress(t)
	cert, key := certPair(t, dir, 32)
	path := configFile(t, dir, a, b, cert, key)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = Ensure(ctx, store, exe, path)
	var ce *Error
	if !errors.As(err, &ce) || ce.Code != "PORT_CONFLICT" {
		t.Fatalf("expected PORT_CONFLICT: %v", err)
	}
	conn, err := net.Dial("tcp4", a)
	if err != nil {
		t.Fatalf("occupied listener was disrupted: %v", err)
	}
	_ = conn.Close()
}
func TestControlCancellation(t *testing.T) {
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	self, err := procid.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.WriteProxy(state.ProxyRecord{Identity: self, State: "ready", ConfigPath: "/tmp/config"}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", store.ProxySocket())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{})
	releaseConn := make(chan struct{})
	go func() {
		conn, e := ln.Accept()
		if e == nil {
			close(accepted)
			<-releaseConn
			_ = conn.Close()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- Reload(ctx, store) }()
	<-accepted
	cancel()
	close(releaseConn)
	if err = <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("reload cancellation lost: %v", err)
	}
	if err = Stop(ctx, store); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop cancellation lost: %v", err)
	}
	if err = Ensure(ctx, store, "unused", "/tmp/config"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ensure cancellation lost: %v", err)
	}
}
func TestAcquireCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	lock, err := acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer release(lock)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		second, e := acquire(ctx, path)
		if e == nil {
			release(second)
		}
		result <- e
	}()
	cancel()
	if err = <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire cancellation lost: %v", err)
	}
}
