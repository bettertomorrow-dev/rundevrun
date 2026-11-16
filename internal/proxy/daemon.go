// Runs the local proxy daemon and maintains its identity.
package proxy

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

type candidate struct {
	config  routing.Config
	tls     *tls.Config
	handler http.Handler
}

func ValidateConfiguration(store *state.Store, path string) (routing.Config, error) {
	next, err := buildCandidate(store, path)
	if err != nil {
		return routing.Config{}, err
	}
	return next.config, nil
}

func buildCandidate(store *state.Store, path string) (*candidate, error) {
	cfg, err := routing.Load(path)
	if err != nil {
		return nil, err
	}
	if !cfg.Routing.Enabled {
		return nil, &Error{Code: "CONFIG_INVALID", Message: "routing is disabled"}
	}
	reg, err := store.Read()
	if err != nil {
		return nil, err
	}
	var hosts []string
	seen := map[string]bool{}
	for id := range reg.Projects {
		project, e := reg.Project(id)
		if e != nil {
			return nil, e
		}
		for _, instance := range project.Instances {
			if instance.Hostname == "" {
				continue
			}
			if seen[instance.Hostname] {
				return nil, &Error{Code: "ROUTE_CONFLICT", Message: "duplicate registered hostname"}
			}
			seen[instance.Hostname] = true
			hosts = append(hosts, instance.Hostname)
		}
	}
	var tlsConfig *tls.Config
	if cfg.ExternalTLS() {
		err = routing.ValidateHosts(cfg, hosts)
	} else {
		tlsConfig, err = routing.LoadTLS(cfg, hosts)
	}
	if err != nil {
		return nil, err
	}
	return &candidate{config: cfg, tls: tlsConfig, handler: NewHandler(store, cfg)}, nil
}

func Serve(root, configPath string) (result error) {
	store, err := state.New(root)
	if err != nil {
		return err
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	lock, err := acquire(ctx, filepath.Join(root, "proxy.lock"))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return busy("proxy is already running")
		}
		return err
	}
	defer release(lock)
	if old, e := store.ReadProxy(); e == nil {
		live, e := procid.Check(old.Identity)
		if e != nil || live {
			return busy("proxy identity is already live or uncertain")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return busy("cannot read previous proxy identity")
	}
	self, err := procid.Observe(os.Getpid())
	if err != nil {
		return err
	}
	record := state.ProxyRecord{Identity: self, State: "starting", ConfigPath: configPath}
	if err = store.WriteProxy(record); err != nil {
		return err
	}
	defer func() {
		if result != nil {
			record.State = "failed"
			record.ErrorCode, record.ErrorMessage = errorReply(result)
		} else {
			record.State = "stopped"
		}
		if err := store.WriteProxy(record); err != nil && result == nil {
			result = err
		}
	}()
	logFile, err := runner.OpenProxyLog(filepath.Join(root, "proxy.log"))
	if err != nil {
		return err
	}
	defer logFile.Close()
	logger := log.New(logFile, "", log.LstdFlags)
	initial, err := buildCandidate(store, configPath)
	if err != nil {
		return err
	}
	httpListener, err := net.Listen("tcp", initial.config.Routing.HTTPListen)
	if err != nil {
		return err
	}
	defer httpListener.Close()
	var httpsListener net.Listener
	if !initial.config.ExternalTLS() {
		httpsListener, err = net.Listen("tcp", initial.config.Routing.HTTPSListen)
		if err != nil {
			return err
		}
		defer httpsListener.Close()
	}
	socket := store.ProxySocket()
	if err = removeSocket(socket); err != nil {
		return err
	}
	controlListener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	controlListener.(*net.UnixListener).SetUnlinkOnClose(false)
	defer removeSocket(socket)
	defer controlListener.Close()
	if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	var active atomic.Pointer[candidate]
	active.Store(initial)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { active.Load().handler.ServeHTTP(w, r) })
	serverHTTP := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ErrorLog: logger}
	serverTLS := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ErrorLog: logger}
	liveTLS := &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { return active.Load().tls, nil }}
	stop := make(chan struct{})
	var once sync.Once
	var reload sync.Mutex
	info := Info{State: "ready", Identity: self, ConfigPath: configPath, HTTPListen: initial.config.Routing.HTTPListen, TLSMode: "provided"}
	if initial.config.ExternalTLS() {
		info.TLSMode = "external"
	} else {
		info.HTTPSListen = initial.config.Routing.HTTPSListen
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(reply{Info: info}) })
	mux.HandleFunc("/reload", func(w http.ResponseWriter, r *http.Request) {
		reload.Lock()
		defer reload.Unlock()
		changed := func(cfg routing.Config) bool {
			return cfg.Routing.HTTPListen != initial.config.Routing.HTTPListen || cfg.Routing.HTTPSListen != initial.config.Routing.HTTPSListen || cfg.ExternalTLS() != initial.config.ExternalTLS()
		}
		cfg, e := routing.Load(configPath)
		if e == nil && changed(cfg) {
			e = &Error{Code: "CONFIG_INVALID", Message: "TLS mode or listener addresses changed; restart proxy"}
		}
		var next *candidate
		if e == nil {
			next, e = buildCandidate(store, configPath)
		}
		if e == nil && changed(next.config) {
			e = &Error{Code: "CONFIG_INVALID", Message: "TLS mode or listener addresses changed; restart proxy"}
		}
		if e != nil {
			code, message := errorReply(e)
			_ = json.NewEncoder(w).Encode(reply{Info: info, Code: code, Message: message})
			return
		}
		active.Store(next)
		_ = json.NewEncoder(w).Encode(reply{Info: info})
	})
	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(reply{Info: info})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		once.Do(func() { close(stop) })
	})
	serverControl := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ErrorLog: logger}
	record.State = "ready"
	if err = store.WriteProxy(record); err != nil {
		return err
	}
	errs := make(chan error, 3)
	go func() { errs <- serverHTTP.Serve(httpListener) }()
	if httpsListener != nil {
		go func() { errs <- serverTLS.Serve(tls.NewListener(httpsListener, liveTLS)) }()
	}
	go func() { errs <- serverControl.Serve(controlListener) }()
	defer serverHTTP.Close()
	defer serverTLS.Close()
	defer serverControl.Close()
	select {
	case <-stop:
		return nil
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("proxy listener: %w", err)
	}
}

func removeSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("unsafe proxy control path: expected a socket")
	}
	return os.Remove(path)
}
