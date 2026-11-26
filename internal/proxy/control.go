// Starts and controls the local routing proxy.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
	"golang.org/x/sys/unix"
)

type Error struct {
	Code     string
	Message  string
	Recovery []string
}

func (e *Error) Error() string     { return e.Code + ": " + e.Message }
func (e *Error) Actions() []string { return e.Recovery }
func busy(message string) error    { return &Error{Code: "BUSY", Message: message} }
func contextOrBusy(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return busy(message)
}

type Info struct {
	State                        string     `json:"state"`
	Identity                     procid.Ref `json:"identity"`
	ConfigPath                   string     `json:"config_path"`
	HTTPListen                   string     `json:"http_listen,omitempty"`
	HTTPSListen                  string     `json:"https_listen,omitempty"`
	TLSMode                      string     `json:"tls_mode,omitempty"`
	TLSError                     string     `json:"tls_error,omitempty"`
	PreparedListen               string     `json:"prepared_listen,omitempty"`
	RenewalError                 string     `json:"renewal_error,omitempty"`
	CertificateActivationPending bool       `json:"certificate_activation_pending,omitempty"`
}
type reply struct {
	Info    Info   `json:"info"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func control(ctx context.Context, store *state.Store, action string) (Info, error) {
	caller := ctx
	if err := caller.Err(); err != nil {
		return Info{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	record, err := store.ReadProxy()
	if errors.Is(err, os.ErrNotExist) {
		return Info{State: "stopped"}, nil
	}
	if err != nil {
		return Info{}, busy("cannot read proxy identity")
	}
	live, err := procid.Check(record.Identity)
	if err != nil {
		return Info{}, busy("cannot verify proxy identity")
	}
	if !live {
		return Info{State: "stopped"}, nil
	}
	if record.State != "ready" {
		return Info{}, busy("proxy is starting or stopping")
	}
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", store.ProxySocket())
	}}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://proxy/"+action, nil)
	if err != nil {
		return Info{}, err
	}
	response, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return Info{}, contextOrBusy(caller, "proxy control unavailable")
	}
	defer response.Body.Close()
	var result reply
	if err = json.NewDecoder(http.MaxBytesReader(nil, response.Body, 64*1024)).Decode(&result); err != nil {
		return Info{}, contextOrBusy(caller, "invalid proxy control response")
	}
	if err := caller.Err(); err != nil {
		return Info{}, err
	}
	if action != "stop" {
		again, e := procid.Check(record.Identity)
		if e != nil || !again {
			return Info{}, busy("proxy identity changed during control request")
		}
	}
	if result.Info.Identity != record.Identity {
		return Info{}, busy("proxy control identity mismatch")
	}
	if result.Code != "" {
		return result.Info, &Error{Code: result.Code, Message: result.Message}
	}
	return result.Info, nil
}
func Status(store *state.Store) (Info, error) {
	return control(context.Background(), store, "status")
}

func InspectStatus(ctx context.Context, store *state.Store, configPath string) (Info, error) {
	info, err := control(ctx, store, "status")
	if (info.State == "stopped" || err != nil) && ctx.Err() == nil {
		if record, err := store.ReadProxy(); err == nil {
			configPath = record.ConfigPath
		}
		if stale, conflict := unrecognizedListener(ctx, configPath); conflict != nil {
			return stale, conflict
		}
	}
	return info, err
}

func PrepareReloadTo(ctx context.Context, store *state.Store, listen string) error {
	info, err := control(ctx, store, "prepare?listen="+url.QueryEscape(listen))
	if err != nil {
		return err
	}
	if info.State != "ready" {
		return &Error{Code: "NOT_FOUND", Message: "proxy is not running"}
	}
	return nil
}

func PrepareReload(ctx context.Context, store *state.Store) error {
	info, err := control(ctx, store, "prepare")
	if err != nil {
		return err
	}
	if info.State != "ready" {
		return &Error{Code: "NOT_FOUND", Message: "proxy is not running"}
	}
	return nil
}
func AbortReload(ctx context.Context, store *state.Store) error {
	_, err := control(ctx, store, "abort")
	return err
}

func reloadRenewal(ctx context.Context, store *state.Store) error {
	info, err := control(ctx, store, "reload-renewal")
	if err != nil {
		return err
	}
	if info.State != "ready" {
		return &Error{Code: "NOT_FOUND", Message: "proxy is not running"}
	}
	if info.TLSError != "" {
		return fmt.Errorf("managed TLS activation: %s", info.TLSError)
	}
	if info.TLSMode != "managed" || info.HTTPSListen == "" {
		return errors.New("managed TLS listener is not active")
	}
	return nil
}

func Reload(ctx context.Context, store *state.Store) error {
	info, err := control(ctx, store, "reload")
	if err != nil {
		return err
	}
	if info.State != "ready" {
		return &Error{Code: "NOT_FOUND", Message: "proxy is not running"}
	}
	return nil
}
func Stop(ctx context.Context, store *state.Store) error {
	caller := ctx
	if err := caller.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	info, err := control(ctx, store, "stop")
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return contextOrBusy(caller, "proxy control timed out")
	}
	if err != nil || info.State == "stopped" {
		return err
	}
	for {
		live, err := procid.Check(info.Identity)
		if err != nil {
			return busy("cannot verify proxy shutdown")
		}
		if !live {
			return nil
		}
		select {
		case <-ctx.Done():
			return contextOrBusy(caller, "proxy did not stop")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func acquire(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		f.Close()
		return nil, errors.New("unsafe proxy lock")
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			if err := ctx.Err(); err != nil {
				release(f)
				return nil, err
			}
			return f, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
func release(f *os.File) { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }

func Ensure(ctx context.Context, store *state.Store, exe, configPath string) error {
	caller := ctx
	if err := caller.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := state.New(store.Root); err != nil {
		return err
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	lock, err := acquire(ctx, filepath.Join(store.Root, "proxy-start.lock"))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return contextOrBusy(caller, "proxy startup is busy")
		}
		return err
	}
	defer release(lock)
	listenerError := func(fallback error) error {
		if _, conflict := unrecognizedListener(ctx, abs); conflict != nil {
			conflict.Code = "PORT_CONFLICT"
			return conflict
		}
		return fallback
	}
	record, err := store.ReadProxy()
	if err == nil {
		live, e := procid.Check(record.Identity)
		if e != nil {
			return listenerError(busy("cannot verify existing proxy"))
		}
		if live {
			if record.ConfigPath != abs {
				return busy("proxy uses a different configuration path")
			}
			if !procid.LegacyBootID(record.Identity.BootID) {
				err = Reload(ctx, store)
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return contextOrBusy(caller, "proxy control timed out")
				}
				var controlErr *Error
				if errors.As(err, &controlErr) && controlErr.Code == "BUSY" {
					return listenerError(err)
				}
				return err
			}
			if err = Stop(ctx, store); err != nil {
				return listenerError(err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return listenerError(busy("cannot read existing proxy identity"))
	}
	if err := listenerError(nil); err != nil {
		return err
	}
	cmd := exec.Command(exe, "__rdr_proxy", store.Root, abs)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	null, e := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if e != nil {
		return e
	}
	defer null.Close()
	cmd.Stdin = null
	cmd.Stdout = null
	cmd.Stderr = null
	if err = cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	daemonExited := false
	for {
		current, e := store.ReadProxy()
		if e == nil && current.Identity.PID == pid {
			if current.ErrorCode != "" {
				startupErr := &Error{Code: current.ErrorCode, Message: current.ErrorMessage}
				if current.ErrorCode == "PORT_CONFLICT" {
					return listenerError(startupErr)
				}
				return startupErr
			}
			if current.State == "ready" {
				info, e := control(ctx, store, "status")
				if e == nil && info.Identity == current.Identity {
					return nil
				}

			}
		}
		if daemonExited {
			return listenerError(contextOrBusy(caller, "proxy exited during startup"))
		}
		select {
		case <-exited:
			daemonExited = true
		case <-ctx.Done():
			return contextOrBusy(caller, "proxy startup did not complete")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func errorReply(err error) (string, string) {
	if errors.Is(err, syscall.EADDRINUSE) {
		return "PORT_CONFLICT", "proxy listener address is already in use"
	}
	var re *routing.Error
	if errors.As(err, &re) {
		return re.Code, re.Err.Error()
	}
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Code, ce.Message
	}
	return "IO_ERROR", fmt.Sprint(err)
}
