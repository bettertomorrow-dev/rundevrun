package publication

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/proxy"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/shareauth"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

var quickURL = regexp.MustCompile(`^https://[a-z0-9][a-z0-9-]*\.trycloudflare\.com$`)

type handlerBox struct{ http.Handler }
type urlSink struct {
	mu     sync.Mutex
	tail   string
	output io.Writer
	urls   chan string
	ready  chan struct{}
}

func (s *urlSink) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, e := s.output.Write(data)
	text := s.tail + string(data[:n])
	lines := strings.Split(text, "\n")
	s.tail = lines[len(lines)-1]
	if len(s.tail) > 8192 {
		s.tail = s.tail[len(s.tail)-8192:]
	}
	for _, line := range lines[:len(lines)-1] {
		if strings.Contains(line, "Registered tunnel connection") {
			select {
			case s.ready <- struct{}{}:
			default:
			}
		}
		for _, field := range strings.Fields(line) {
			candidate := strings.Trim(field, "|\"'()[]<>,;")
			if quickURL.MatchString(candidate) && validQuickURL(candidate) {
				select {
				case s.urls <- candidate:
				default:
				}
			}
		}
	}

	return n, e
}

// Bootstrap waits for the supervisor's identity record before executing cloudflared.
func Bootstrap(args []string) error {
	if len(args) == 0 || !filepath.IsAbs(args[0]) {
		return errors.New("invalid tunnel command")
	}
	pipe := os.NewFile(3, "publication-ack")
	if pipe == nil {
		return errors.New("missing authorization pipe")
	}
	var ack [1]byte
	_, e := io.ReadFull(pipe, ack[:])
	e = errors.Join(e, pipe.Close())
	if e != nil || ack[0] != 1 {
		return errors.New("publication authorization denied")
	}
	return syscall.Exec(args[0], args, os.Environ())
}
func cleanTunnelEnv() []string {
	out := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "TUNNEL_") || strings.HasPrefix(upper, "CLOUDFLARED_") || strings.HasPrefix(upper, "CF_") {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// Supervise owns a single loopback gateway and its cloudflared child.
func Supervise(root, id string) (result error) {
	store := &state.Store{Root: root}
	gate, e := store.LockPublicationGate(id)
	if e != nil {
		return e
	}
	gateHeld := true
	defer func() {
		if gateHeld {
			_ = gate.Close()
		}
	}()
	registry, e := store.ReadPublications()
	if e != nil {
		return e
	}
	p, ok := registry.Publications[id]
	if !ok || p.Phase != "preparing" {
		return errors.New("publication authorization revoked")
	}
	ownerLive, e := procid.Check(p.Owner)
	if e != nil || !ownerLive {
		return errors.New("publication creator is not live")
	}
	self, e := procid.Observe(os.Getpid())
	if e != nil {
		return e
	}
	record := state.PublicationRuntime{ID: id, State: "starting", Supervisor: self}
	failureCode := "TUNNEL_FAILED"
	if e = store.WritePublicationRuntime(id, record); e != nil {
		return e
	}
	defer func() {
		if result != nil {
			record.State = "failed"
			record.ErrorCode = failureCode
			record.ErrorMessage = "publication failed; see tunnel log"
			if failureCode == "BUILD_FAILED" {
				record.ErrorMessage = "snapshot build or copy failed; see build log"
			}
			if errors.Is(result, errSnapshotTunnelLost) {
				record.ErrorCode = "TUNNEL_FAILED"
				record.ErrorMessage = "tunnel failed during publication preparation"
			}
			if errors.Is(result, context.DeadlineExceeded) {
				record.ErrorCode = "READINESS_TIMEOUT"
			}
		} else {
			record.State = "stopped"
		}
		_ = store.WritePublicationRuntime(id, record)
	}()
	dir, e := store.PublicationDir(id, false)
	if e != nil {
		return e
	}
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return e
	}
	defer listener.Close()
	var handler atomic.Value
	handler.Store(handlerBox{http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "publication starting", http.StatusServiceUnavailable)
	})})
	server := newGatewayServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.Load().(handlerBox).Handler.ServeHTTP(w, r) }))
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	defer server.Close()
	log, e := runner.OpenProxyLog(filepath.Join(dir, "tunnel.log"))
	if e != nil {
		return e
	}
	defer log.Close()
	read, ack, e := os.Pipe()
	if e != nil {
		return e
	}
	defer read.Close()
	defer ack.Close()
	binary, e := os.Executable()
	if e != nil {
		return e
	}
	args := []string{"__rdr_publication_bootstrap", p.Cloudflared, "tunnel", "--no-autoupdate", "--config", filepath.Join(dir, "cloudflared.yml"), "--metrics", "127.0.0.1:0", "--origincert", filepath.Join(dir, "no-origin-cert.pem"), "--url", "http://" + listener.Addr().String()}
	cmd := exec.Command(binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = cleanTunnelEnv()
	cmd.ExtraFiles = []*os.File{read}
	sink := &urlSink{output: log, urls: make(chan string, 1), ready: make(chan struct{}, 1)}
	cmd.Stdout = sink
	cmd.Stderr = sink
	if e = cmd.Start(); e != nil {
		return e
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		if record.Tunnel.PID > 0 {
			procid.Terminate([]procid.Ref{record.Tunnel}, time.Second)
		}
	}()
	read.Close()
	ref, e := procid.Observe(cmd.Process.Pid)
	if e != nil {
		_ = cmd.Process.Kill()
		return e
	}
	record.Tunnel = ref
	record.Members = []procid.Ref{ref}
	if e = store.WritePublicationRuntime(id, record); e != nil {
		_ = cmd.Process.Kill()
		return e
	}
	if _, e = ack.Write([]byte{1}); e != nil {
		_ = cmd.Process.Kill()
		return e
	}
	if e = ack.Close(); e != nil {
		_ = cmd.Process.Kill()
		return e
	}
	if e = gate.Close(); e != nil {
		return e
	}
	gateHeld = false
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	leaseTick := time.NewTicker(100 * time.Millisecond)
	defer leaseTick.Stop()
	lease := func() (bool, error) {
		registry, err := store.ReadPublications()
		if err != nil {
			return false, err
		}
		current, ok := registry.Publications[id]
		if !ok {
			return false, errors.New("publication intent missing")
		}
		switch current.Phase {
		case "active":
			return true, nil
		case "preparing":
			live, err := procid.Check(current.Owner)
			if err != nil {
				return false, err
			}
			if !live {
				return false, errors.New("publication creator exited before activation")
			}
			return true, nil
		case "closing", "closed":
			return false, nil
		default:
			return false, errors.New("publication intent invalid")
		}
	}

	public := ""
	registered := false
	for public == "" || !registered {
		select {
		case candidate := <-sink.urls:
			if public != "" && public != candidate {
				return errors.New("tunnel URL changed during startup")
			}
			if public == "" && p.Mode == "snapshot" {
				public = candidate
			} else if public == "" {
				credentials := shareauth.Record{Salt: p.Salt, Verifier: p.Verifier, SigningKey: p.SigningKey}
				next := proxy.NewPinnedHandler(store, p.ProjectID, p.RootID, p.RootRunID, p.RoutedRuns)
				auth, err := shareauth.NewHandler(id, credentials, candidate, next)
				if err != nil {
					return err
				}
				handler.Store(handlerBox{auth})
				public = candidate
			}
		case <-sink.ready:
			registered = true
		case err := <-done:
			if err == nil {
				return errors.New("tunnel exited before readiness")
			}
			return fmt.Errorf("tunnel exited before readiness: %w", err)
		case <-leaseTick.C:
			ok, err := lease()
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
		case <-timer.C:
			return errors.New("tunnel readiness timeout")
		case <-ctx.Done():
			return nil
		case err := <-serverDone:
			return err
		}
	}
	if p.Mode == "snapshot" {
		failureCode = "BUILD_FAILED"
		if e = buildSnapshot(ctx, store, p, &record, public, dir, lease, done); e != nil {
			return e
		}
		credentials := shareauth.Record{Salt: p.Salt, Verifier: p.Verifier, SigningKey: p.SigningKey}
		next := proxy.NewPinnedSnapshotHandler(store, p.ProjectID, p.RootID, p.RootRunID, p.RoutedRuns, filepath.Join(dir, "snapshot"))
		auth, err := shareauth.NewHandler(id, credentials, public, next)
		if err != nil {
			return err
		}
		handler.Store(handlerBox{auth})
		failureCode = "TUNNEL_FAILED"
	}
	record.URL = public
	record.State = "ready"
	if e = store.WritePublicationRuntime(id, record); e != nil {
		return e
	}

	for {
		select {
		case err := <-done:
			if err == nil {
				return errors.New("tunnel exited")
			}
			return err
		case <-ctx.Done():
			return nil
		case err := <-serverDone:
			return err
		case <-leaseTick.C:
			ok, err := lease()
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
		}
	}
}

// ReadTimeout bounds body reads and net/http's unread-body drain on rejected
// requests. No write deadline: streamed responses may outlive a request body.
// The standard HTTP hijack path clears deadlines for WebSocket connections.
func newGatewayServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second}
}
