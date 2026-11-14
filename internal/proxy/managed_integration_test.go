package proxy

import (
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
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/networking"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestManagedTLSReloadAndAdmission(t *testing.T) {
	// The loader seam avoids changing machine trust; the real daemon/control and TLS listener run below.
	probe, err := net.Listen("tcp", "127.0.0.1:8443")
	if err != nil {
		t.Skipf("TLS fixture port unavailable: %v", err)
	}
	probe.Close()
	probe, err = net.Listen("tcp", "127.0.0.1:8444")
	if err != nil {
		t.Skipf("managed fixture port unavailable: %v", err)
	}
	probe.Close()
	local, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := local.Addr().String()
	local.Close()
	root, err := os.MkdirTemp("/tmp", "rdr-tls-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	configRoot := filepath.Join(root, "config")
	if err = os.Mkdir(configRoot, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := networking.DefaultConfig()
	cfg.Local.Listen = address
	cfg.Tailnet.Domain = "tail.example.com"
	cfg.Projects["p"] = networking.Project{Tailnet: false}
	cfg.Projects["q"] = networking.Project{Tailnet: true}
	encoded, _ := json.Marshal(cfg)
	if err = os.WriteFile(filepath.Join(configRoot, "networking.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := state.New(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Update(func(reg *state.Registry) error {
		for _, name := range []string{"p", "q"} {
			id := name + "/w/web"
			instance := state.Instance{ID: id, WorktreeID: "w", Service: "web", State: "stopped", Endpoints: []state.Endpoint{{Scope: "tailnet", Hostname: name + ".tail.example.com", Port: 443, InstanceID: id}, {Scope: "local", Hostname: name + ".localhost", Port: 8080, InstanceID: id}}}
			if name == "p" {
				instance.Hostname = "p.dev.example.com"
				instance.RouteURL = "https://p.dev.example.com:8443"
			}
			if err := reg.SetProject(name, state.Project{ID: name, Name: name, Root: "/tmp/" + name, Worktrees: []state.Worktree{{ID: "w", Root: "/tmp/" + name}}, Instances: map[string]state.Instance{id: instance}}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.RWMutex
	mkcert := func(serial int64) (*tls.Certificate, *x509.Certificate) {
		t.Helper()
		now := time.Now()
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{"*.dev.example.com", "*.tail.example.com"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, e := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if e != nil {
			t.Fatal(e)
		}
		pk, _ := x509.MarshalPKCS8PrivateKey(key)
		pair, e := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}))
		if e != nil {
			t.Fatal(e)
		}
		leaf, e := x509.ParseCertificate(der)
		if e != nil {
			t.Fatal(e)
		}
		pair.Leaf = leaf
		return &pair, leaf
	}
	cert, ca := mkcert(1)
	cert2, ca2 := mkcert(2)
	private, _ := x509.MarshalPKCS8PrivateKey(key)
	if err = os.WriteFile(filepath.Join(configRoot, "legacy.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(configRoot, "legacy-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0600); err != nil {
		t.Fatal(err)
	}
	legacy := fmt.Sprintf("version = 1\n[routing]\nenabled = true\ndomain = \"dev.example.com\"\nhttp_listen = \"%s\"\nhttps_listen = \"127.0.0.1:8443\"\n[[routing.tls.certificates]]\ncert_file = \"legacy.pem\"\nkey_file = \"legacy-key.pem\"\n", address)
	if err = os.WriteFile(filepath.Join(configRoot, "config.toml"), []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	oldLoader := loadManagedCertificate
	loadManagedCertificate = func(networking.Service, string) (*tls.Certificate, error) {
		mu.RLock()
		defer mu.RUnlock()
		return cert, nil
	}
	defer func() { loadManagedCertificate = oldLoader }()
	done := make(chan error, 1)
	go func() { done <- Serve(store.Root, filepath.Join(configRoot, "config.toml")) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, e := Status(store)
		if e == nil && info.State == "ready" {
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
		_, _ = control(ctx, store, "stop")
		select {
		case e := <-done:
			if e != nil {
				t.Errorf("proxy: %v", e)
			}
		case <-ctx.Done():
			t.Error("proxy did not exit")
		}
	}()
	checkLocal := func() {
		t.Helper()
		plain, err := http.NewRequest("GET", "http://"+address+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		plain.Host = "p.localhost"
		response, err := http.DefaultClient.Do(plain)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 503 {
			t.Fatalf("local with legacy status=%d", response.StatusCode)
		}
	}
	checkLocal()
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	roots.AddCert(ca2)
	request := func(host, addr string) (int, int64) {
		t.Helper()
		dialer := &net.Dialer{Timeout: 2 * time.Second}
		tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: host}, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", addr)
		}}
		defer tr.CloseIdleConnections()
		client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
		resp, e := client.Get("https://" + host + "/")
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		return resp.StatusCode, resp.TLS.PeerCertificates[0].SerialNumber.Int64()
	}
	if status, serial := request("q.tail.example.com", "127.0.0.1:8444"); status != 503 || serial != 1 {
		t.Fatal(fmt.Sprintf("allowed status=%d serial=%d", status, serial))
	}
	if status, _ := request("p.dev.example.com", "127.0.0.1:8444"); status != 404 {
		t.Fatalf("opted-out status=%d", status)
	}
	if status, serial := request("p.dev.example.com", "127.0.0.1:8443"); status != 503 || serial != 1 {
		t.Fatalf("legacy listener: status=%d serial=%d", status, serial)
	}
	// A real outstanding listener reservation forces the first certificate
	// activation to fail after a replacement was published. The retry must
	// activate that replacement without calling issuance again.
	// Even a reservation for the currently active address belongs to the
	// interactive operation and must not be consumed by background renewal.
	reserved := address
	if err = PrepareReloadTo(context.Background(), store, reserved); err != nil {
		t.Fatal(err)
	}
	cycle := renewalCycle{}
	issuanceCount := 0
	renew := func(context.Context) (bool, error) {
		issuanceCount++
		mu.Lock()
		cert = cert2
		mu.Unlock()
		return true, nil
	}
	activate := func(ctx context.Context) error { return reloadRenewal(ctx, store) }
	err = cycle.step(context.Background(), renew, activate)
	var conflict *Error
	if !errors.As(err, &conflict) || conflict.Code != "BUSY" || !cycle.activationPending || issuanceCount != 1 {
		t.Fatalf("failed activation state: err=%v pending=%v issuance=%d", err, cycle.activationPending, issuanceCount)
	}
	infoBeforeAbort, err := Status(store)
	if err != nil || infoBeforeAbort.PreparedListen != reserved {
		t.Fatalf("renewal consumed reservation: %+v %v", infoBeforeAbort, err)
	}
	if status, serial := request("q.tail.example.com", "127.0.0.1:8444"); status != 503 || serial != 1 {
		t.Fatalf("previous cert lost: status=%d serial=%d", status, serial)
	}
	checkLocal()
	if err = AbortReload(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if err = cycle.step(context.Background(), renew, activate); err != nil {
		t.Fatal(err)
	}
	if cycle.activationPending || issuanceCount != 1 {
		t.Fatalf("retry reissued or remained pending: %d", issuanceCount)
	}
	if status, serial := request("q.tail.example.com", "127.0.0.1:8444"); status != 503 || serial != 2 {
		t.Fatalf("retried activation: status=%d serial=%d", status, serial)
	}
	checkLocal()
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proposed := free.Addr().String()
	free.Close()
	if err = PrepareReloadTo(context.Background(), store, proposed); err != nil {
		t.Fatal(err)
	}
	info, err := Status(store)
	if err != nil || info.PreparedListen != proposed {
		t.Fatalf("reservation: %+v %v", info, err)
	}
	if err = PrepareReloadTo(context.Background(), store, proposed); err == nil {
		t.Fatal("accepted concurrent reservation")
	}
	if err = AbortReload(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	info, err = Status(store)
	if err != nil || info.PreparedListen != "" {
		t.Fatalf("reservation after abort: %+v %v", info, err)
	}
}
