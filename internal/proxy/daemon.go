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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/localrouting"
	"github.com/bettertomorrow-dev/rundevrun/internal/networking"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

var loadManagedCertificate = func(service networking.Service, domain string) (*tls.Certificate, error) {
	return service.ManagedCertificate(domain)
}

type candidate struct {
	config         routing.Config
	tls            *tls.Config
	legacyTLS      *tls.Config
	handler        http.Handler
	managedHandler http.Handler
	legacyHandler  http.Handler
	tlsError       string
}

// ValidateConfiguration builds the same reload candidate without starting or
// changing the daemon. It only reads registry, configuration and certificates.
func ValidateConfiguration(store *state.Store, path string) (routing.Config, error) {
	next, err := buildCandidate(store, path)
	if err != nil {
		return routing.Config{}, err
	}
	return next.config, nil
}

func buildCandidate(store *state.Store, path string) (*candidate, error) {
	cfg, err := localrouting.Load(path)
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
	if cfg.LocalOnly {
		err = routing.ValidateHosts(cfg, hosts)
	} else if cfg.ExternalTLS() {
		err = routing.ValidateHosts(cfg, hosts)
	} else {
		tlsConfig, err = routing.LoadTLS(cfg, hosts)
	}
	if err != nil {
		return nil, err
	}
	next := &candidate{config: cfg, tls: tlsConfig, handler: NewHandler(store, cfg), managedHandler: NewHandler(store, cfg)}
	if cfg.LocalOnly && cfg.TailnetDomain != "" {
		enabled := false
		for _, project := range cfg.LocalProjects {
			if project.Tailnet {
				enabled = true
				break
			}
		}
		if enabled {
			cert, e := loadManagedCertificate(networking.Service{Root: filepath.Dir(path)}, cfg.TailnetDomain)
			if e != nil {
				next.tlsError = e.Error()
			} else {
				next.tls = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*cert}}
			}
		}
	}
	if cfg.LocalOnly && cfg.Legacy != nil {
		var legacyHosts []string
		legacyOwners := map[string]string{}
		for id := range reg.Projects {
			project, e := reg.Project(id)
			if e != nil {
				return nil, e
			}
			for _, instance := range project.Instances {
				if instance.Hostname != "" && !strings.HasSuffix(instance.Hostname, ".localhost") {
					legacyHosts = append(legacyHosts, instance.Hostname)
					legacyOwners[instance.Hostname] = instance.ID
				}
			}
		}

		if cfg.Legacy.ExternalTLS() {
			err = routing.ValidateHosts(*cfg.Legacy, legacyHosts)
		} else {
			next.legacyTLS, err = routing.LoadTLS(*cfg.Legacy, legacyHosts)
		}
		if err != nil {
			next.tlsError = "legacy TLS: " + err.Error()
		}
		localHandler, legacyHandler := next.handler, NewHandler(store, *cfg.Legacy)
		next.legacyHandler = legacyHandler
		next.handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			authority := req.Host
			if cfg.Legacy.ExternalTLS() && req.Header.Get("X-Forwarded-Host") != "" {
				authority = req.Header.Get("X-Forwarded-Host")
			}
			host, e := hostname(authority)
			if e != nil {
				http.Error(w, "bad host", http.StatusBadRequest)
				return
			}
			if legacyOwners[host] != "" {
				legacyHandler.ServeHTTP(w, req)
				return
			}
			if strings.HasSuffix(host, ".localhost") || (cfg.TailnetDomain != "" && strings.HasSuffix(host, "."+cfg.TailnetDomain)) {
				localHandler.ServeHTTP(w, req)
				return
			}
			legacyHandler.ServeHTTP(w, req)
		})
	}
	return next, nil
}

// Serve runs one per-user proxy. Its identity is durable before any listener is
// opened. The lifetime lock is never used as authority to signal a process.
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
	var legacyHTTP net.Listener
	if initial.config.LocalOnly && initial.config.Legacy != nil && initial.config.Legacy.Routing.HTTPListen != initial.config.Routing.HTTPListen {
		legacyHTTP, err = net.Listen("tcp", initial.config.Legacy.Routing.HTTPListen)
		if err != nil {
			initial.tlsError = "legacy HTTP listener: " + err.Error()
		} else {
			defer legacyHTTP.Close()
		}
	}
	var httpsListener net.Listener
	if !initial.config.ExternalTLS() && initial.tls != nil {
		httpsListener, err = net.Listen("tcp", initial.config.Routing.HTTPSListen)
		if err != nil {
			if !initial.config.LocalOnly {
				return err
			}
			initial.tlsError = "tailnet listener: " + err.Error()
			initial.tls = nil
		}
	}
	var legacyHTTPS net.Listener
	if initial.config.Legacy != nil && initial.legacyTLS != nil {
		legacyHTTPS, err = net.Listen("tcp", initial.config.Legacy.Routing.HTTPSListen)
		if err != nil {
			initial.tlsError = "legacy HTTPS listener: " + err.Error()
		} else {
			defer legacyHTTPS.Close()
		}
	}
	defer func() {
		if httpsListener != nil {
			_ = httpsListener.Close()
		}
	}()
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
	managedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { active.Load().managedHandler.ServeHTTP(w, r) })
	legacyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := active.Load().legacyHandler
		if h == nil {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
	serverTLS := &http.Server{Handler: managedHandler, ReadHeaderTimeout: 10 * time.Second, ErrorLog: logger}
	serverLegacyTLS := &http.Server{Handler: legacyHandler, ReadHeaderTimeout: 10 * time.Second, ErrorLog: logger}
	liveTLS := &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		config := active.Load().tls
		if config == nil {
			return nil, errors.New("managed TLS unavailable")
		}
		return config, nil
	}}
	legacyLiveTLS := &tls.Config{MinVersion: tls.VersionTLS12, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		config := active.Load().legacyTLS
		if config == nil {
			return nil, errors.New("legacy TLS unavailable")
		}
		return config, nil
	}}
	stop := make(chan struct{})
	var once sync.Once
	var reload sync.Mutex
	var prepared net.Listener
	var preparedAddress string
	var preparedActive bool
	clearPrepared := func() {
		if prepared != nil {
			_ = prepared.Close()
			prepared = nil
		}
		preparedAddress = ""
		preparedActive = false
	}
	defer func() { reload.Lock(); clearPrepared(); reload.Unlock() }()
	info := Info{State: "ready", Identity: self, ConfigPath: configPath, HTTPListen: initial.config.Routing.HTTPListen, TLSMode: "provided"}
	if initial.config.LocalOnly {
		info.TLSMode = "local"
		info.TLSError = initial.tlsError
		if httpsListener != nil && initial.tls != nil {
			info.TLSMode = "managed"
			info.HTTPSListen = initial.config.Routing.HTTPSListen
		}
	} else if initial.config.ExternalTLS() {
		info.TLSMode = "external"
	} else {
		info.HTTPSListen = initial.config.Routing.HTTPSListen
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		reload.Lock()
		defer reload.Unlock()
		snapshot := info
		if preparedActive {
			snapshot.PreparedListen = preparedAddress
		}
		_ = json.NewEncoder(w).Encode(reply{Info: snapshot})
	})
	mux.HandleFunc("/prepare", func(w http.ResponseWriter, r *http.Request) {
		reload.Lock()
		defer reload.Unlock()
		if preparedActive {
			_ = json.NewEncoder(w).Encode(reply{Info: info, Code: "BUSY", Message: "another listener change is being prepared"})
			return
		}
		cfg, e := localrouting.Load(configPath)
		address := r.URL.Query().Get("listen")
		if address == "" {
			address = cfg.Routing.HTTPListen
		}
		host, port, parseErr := net.SplitHostPort(address)
		n, portErr := strconv.Atoi(port)
		if parseErr != nil || portErr != nil || n < 1024 || n > 65535 || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			e = &Error{Code: "CONFIG_INVALID", Message: "local listener must use a loopback IP and unprivileged port"}
		}
		if e == nil && cfg.LocalOnly && address != active.Load().config.Routing.HTTPListen {
			prepared, e = net.Listen("tcp", address)
		}
		if e == nil {
			preparedAddress = address
			preparedActive = true
		}
		if e != nil {
			code, message := errorReply(e)
			_ = json.NewEncoder(w).Encode(reply{Info: info, Code: code, Message: message})
			return
		}
		_ = json.NewEncoder(w).Encode(reply{Info: info})
	})
	mux.HandleFunc("/abort", func(w http.ResponseWriter, r *http.Request) {
		reload.Lock()
		defer reload.Unlock()
		clearPrepared()
		_ = json.NewEncoder(w).Encode(reply{Info: info})
	})
	reloadAction := func(w http.ResponseWriter, r *http.Request) {
		reload.Lock()
		defer reload.Unlock()
		if r.URL.Path == "/reload-renewal" && preparedActive {
			_ = json.NewEncoder(w).Encode(reply{Info: info, Code: "BUSY", Message: "another listener change is being prepared"})
			return
		}
		changed := func(cfg routing.Config) bool {
			current := active.Load().config
			if current.Routing.HTTPSListen != cfg.Routing.HTTPSListen {
				return true
			}
			return (!cfg.LocalOnly || !current.LocalOnly) && (cfg.Routing.HTTPListen != current.Routing.HTTPListen || cfg.Routing.HTTPSListen != current.Routing.HTTPSListen || cfg.ExternalTLS() != current.ExternalTLS() || cfg.LocalOnly != current.LocalOnly)
		}
		cfg, e := localrouting.Load(configPath)
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
		if e == nil && preparedActive && next.config.Routing.HTTPListen != preparedAddress {
			e = &Error{Code: "STATE_CONFLICT", Message: "network settings changed after listener preparation"}
		}
		if e != nil {
			code, message := errorReply(e)
			_ = json.NewEncoder(w).Encode(reply{Info: info, Code: code, Message: message})
			return
		}
		if next.config.LocalOnly && next.tls == nil && next.tlsError != "" {
			old := active.Load()
			if old.config.TailnetDomain == next.config.TailnetDomain && old.tls != nil && len(old.tls.Certificates) > 0 {
				leaf := old.tls.Certificates[0].Leaf
				enabled := false
				for _, project := range next.config.LocalProjects {
					if project.Tailnet {
						enabled = true
						break
					}
				}
				if enabled && leaf != nil && time.Now().Before(leaf.NotAfter) {
					next.tls = old.tls
				}
			}
		}
		if next.config.LocalOnly {
			if next.tls != nil && httpsListener == nil {
				listener, bindErr := net.Listen("tcp", next.config.Routing.HTTPSListen)
				if bindErr != nil {
					next.tlsError = "tailnet listener: " + bindErr.Error()
					next.tls = nil
				} else {
					httpsListener = listener
					go func() { _ = serverTLS.Serve(tls.NewListener(listener, liveTLS)) }()
				}
			} else if next.tls == nil && httpsListener != nil {
				_ = httpsListener.Close()
				httpsListener = nil
			}
			info.TLSMode, info.HTTPSListen, info.TLSError = "local", "", next.tlsError
			if httpsListener != nil {
				info.TLSMode = "managed"
				info.HTTPSListen = next.config.Routing.HTTPSListen
			}
		}
		if next.config.LocalOnly && next.config.Routing.HTTPListen != active.Load().config.Routing.HTTPListen {
			var listener net.Listener
			var bindErr error
			if prepared != nil && preparedAddress == next.config.Routing.HTTPListen {
				listener = prepared
				prepared = nil
				clearPrepared()
			} else {
				clearPrepared()
				listener, bindErr = net.Listen("tcp", next.config.Routing.HTTPListen)
			}
			if bindErr != nil {
				code, message := errorReply(bindErr)
				_ = json.NewEncoder(w).Encode(reply{Info: info, Code: code, Message: message})
				return
			}
			previous := httpListener
			httpListener = listener
			go func() { _ = serverHTTP.Serve(listener) }()
			active.Store(next)
			info.HTTPListen = next.config.Routing.HTTPListen
			_ = previous.Close()
		} else {
			active.Store(next)
			clearPrepared()
		}
		_ = json.NewEncoder(w).Encode(reply{Info: info})
	}
	mux.HandleFunc("/reload", reloadAction)
	mux.HandleFunc("/reload-renewal", reloadAction)
	mux.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		reload.Lock()
		defer reload.Unlock()
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
	if legacyHTTP != nil {
		go func() { _ = serverHTTP.Serve(legacyHTTP) }()
	}
	if legacyHTTPS != nil {
		go func() { _ = serverLegacyTLS.Serve(tls.NewListener(legacyHTTPS, legacyLiveTLS)) }()
	}
	go func(listener net.Listener) {
		if err := serverHTTP.Serve(listener); err != nil && !errors.Is(err, net.ErrClosed) {
			errs <- err
		}
	}(httpListener)
	if httpsListener != nil {
		go func(listener net.Listener) {
			if err := serverTLS.Serve(tls.NewListener(listener, liveTLS)); err != nil && !errors.Is(err, net.ErrClosed) {
				errs <- err
			}
		}(httpsListener)
	}
	go func() { errs <- serverControl.Serve(controlListener) }()
	renewalCtx, cancelRenewal := context.WithCancel(context.Background())
	defer cancelRenewal()
	go func() {
		timer := time.NewTimer(0)
		backoff := time.Minute
		cycle := &renewalCycle{}
		defer timer.Stop()
		for {
			select {
			case <-renewalCtx.Done():
				return
			case <-timer.C:
				ctx, cancel := context.WithTimeout(renewalCtx, 3*time.Minute)
				err := cycle.step(ctx, (networking.Service{Root: filepath.Dir(configPath)}).RenewIfNeeded, func(ctx context.Context) error {
					activationCtx, activationCancel := context.WithTimeout(ctx, 10*time.Second)
					defer activationCancel()
					return reloadRenewal(activationCtx, store)
				})
				cancel()
				reload.Lock()
				info.CertificateActivationPending = cycle.activationPending
				info.RenewalError = ""
				if err != nil {
					info.RenewalError = err.Error()
				}
				reload.Unlock()
				if err != nil {
					logger.Print(err)
					timer.Reset(backoff)
					backoff *= 2
					if backoff > time.Hour {
						backoff = time.Hour
					}
					continue
				}
				backoff = time.Minute
				timer.Reset(time.Hour)
			}
		}
	}()
	defer serverHTTP.Close()
	defer serverTLS.Close()
	defer serverLegacyTLS.Close()
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

// removeSocket only removes a socket while the daemon holds its lifetime lock.
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
