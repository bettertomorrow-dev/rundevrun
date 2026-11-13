package publication

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"syscall"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/shareauth"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
	"golang.org/x/sys/unix"
)

var quickHost = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?\.trycloudflare\.com$`)

func validQuickURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.Host == u.Hostname() && quickHost.MatchString(u.Host)
}
func newID() (string, error) {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}
func publicStatus(p state.Publication, rt state.PublicationRuntime) Status {
	s := Status{ID: p.ID, Mode: p.Mode, State: rt.State, URL: rt.URL, Root: Target{p.RootID, p.RootRunID}}
	for id, run := range p.RoutedRuns {
		s.Routed = append(s.Routed, Target{id, run})
	}
	sort.Slice(s.Routed, func(i, j int) bool { return s.Routed[i].InstanceID < s.Routed[j].InstanceID })
	return s
}

// List returns only public-safe fields and does not create state.
func (m *Manager) List(ctx context.Context) ([]Status, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if m == nil || m.Store == nil {
		return nil, &runner.Error{Code: "USAGE", Err: errors.New("missing publication manager")}
	}
	registry, e := m.Store.ReadPublications()
	if e != nil {
		return nil, e
	}
	ids := make([]string, 0, len(registry.Publications))
	for id, p := range registry.Publications {
		if p.Phase != "closed" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	result := make([]Status, 0, len(ids))
	for _, id := range ids {
		p := registry.Publications[id]
		ownerLive := true
		if p.Phase == "preparing" {
			var checkErr error
			ownerLive, checkErr = procid.Check(p.Owner)
			if checkErr != nil {
				return nil, &runner.Error{Code: "OWNERSHIP_UNKNOWN", Err: checkErr}
			}
		}
		rt, err := m.Store.ReadPublicationRuntime(id)
		missingRuntime := errors.Is(err, os.ErrNotExist)
		if errors.Is(err, os.ErrNotExist) && p.Phase == "preparing" {
			rt = state.PublicationRuntime{ID: id, State: "starting"}
			if !ownerLive {
				rt.State = "failed"
			}
		} else if err != nil {
			return nil, &runner.Error{Code: "OWNERSHIP_UNKNOWN", Err: fmt.Errorf("publication %s runtime unavailable: %w", id, err)}
		}
		status := publicStatus(p, rt)
		if p.Phase == "preparing" && status.State == "ready" {
			status.State = "starting"
			status.URL = ""
		}
		if !missingRuntime && rt.Supervisor == (procid.Ref{}) {
			return nil, &runner.Error{Code: "OWNERSHIP_UNKNOWN", Err: errors.New("publication supervisor identity missing")}
		}
		if p.Phase == "preparing" && !ownerLive {
			status.State = "failed"
			status.URL = ""
		}
		if p.Phase == "closing" {
			status.State = "closing"
			status.URL = ""
		}
		if p.Phase == "failed" {
			status.State = "failed"
			status.URL = ""
		}
		if rt.Supervisor != (procid.Ref{}) {
			live, checkErr := procid.Check(rt.Supervisor)
			if checkErr != nil {
				return nil, &runner.Error{Code: "OWNERSHIP_UNKNOWN", Err: checkErr}
			}
			if !live {
				status.State = "failed"
				status.URL = ""
			}
		}
		if rt.State == "ready" {
			live, checkErr := procid.Check(rt.Tunnel)
			if checkErr != nil {
				return nil, &runner.Error{Code: "OWNERSHIP_UNKNOWN", Err: checkErr}
			}
			if !live {
				status.State = "failed"
				status.URL = ""
			}
			if !validQuickURL(rt.URL) {
				return nil, fmt.Errorf("%w: invalid publication URL", state.ErrCorrupt)
			}
		}
		result = append(result, status)
	}
	return result, nil
}
func selectionID(selection runner.Selection) (string, error) {
	if selection.Project == nil || selection.Worktree == nil || selection.Service == "" {
		return "", &runner.Error{Code: "USAGE", Err: errors.New("missing publication selection")}
	}
	return selection.Project.ID + "/" + selection.Worktree.ID + "/" + selection.Service, nil
}
func (m *Manager) validateSelection(selection runner.Selection) (string, error) {
	id, e := selectionID(selection)
	if e != nil {
		return "", e
	}
	if m == nil || m.Store == nil {
		return "", &runner.Error{Code: "USAGE", Err: errors.New("missing publication manager")}
	}
	reg, e := m.Store.Read()
	if e != nil {
		return "", e
	}
	p, e := reg.Project(selection.Project.ID)
	if e != nil {
		return "", e
	}
	for _, w := range p.Worktrees {
		if w.ID == selection.Worktree.ID && w.Root == selection.Worktree.Root {
			if _, ok := p.Instances[id]; !ok {
				return "", &runner.Error{Code: "NOT_FOUND", Err: errors.New("instance not registered")}
			}
			return id, nil
		}
	}
	return "", &runner.Error{Code: "NOT_FOUND", Err: errors.New("worktree not registered")}
}
func (m *Manager) Status(ctx context.Context, selection runner.Selection) (Status, error) {
	id, e := m.validateSelection(selection)
	if e != nil {
		return Status{}, e
	}
	all, e := m.List(ctx)
	if e != nil {
		return Status{}, e
	}
	var found *Status
	for _, item := range all {
		if item.Root.InstanceID == id {
			if found != nil {
				return Status{}, &runner.Error{Code: "BUSY", Err: errors.New("multiple publications exist; list and unshare them")}
			}
			copy := item
			found = &copy
		}
	}
	if found == nil {
		return Status{State: "stopped", Root: Target{InstanceID: id}}, nil
	}
	return *found, nil
}

func (m *Manager) Share(ctx context.Context, selection runner.Selection, options Options) (result Result, retErr error) {
	if options.Mode != "" && options.Mode != "live" {
		return result, &runner.Error{Code: "CONFIG_INVALID", Err: errors.New("only live publication is supported")}
	}
	plan, e := m.PlanLive(ctx, selection)
	if e != nil {
		return result, e
	}
	locks, e := m.Store.LockInstances(plan.Locks...)
	if e != nil {
		return result, e
	}
	defer locks.Close()
	again, e := m.PlanLive(ctx, selection)
	if e != nil {
		return result, e
	}
	if !reflect.DeepEqual(plan, again) {
		return result, &runner.Error{Code: "STATE_CONFLICT", Err: errors.New("publication targets changed while locking")}
	}
	prior, e := m.Status(ctx, selection)
	if e != nil {
		return result, e
	}
	if prior.ID != "" && prior.State != "ready" {
		return result, &runner.Error{Code: "BUSY", Err: errors.New("existing publication needs explicit unshare")}
	}
	if prior.ID != "" && !options.Recreate {
		if prior.State != "ready" {
			return result, &runner.Error{Code: "BUSY", Err: errors.New("existing publication needs explicit unshare")}
		}
		return Result{Status: prior}, nil
	}
	executable, e := exec.LookPath("cloudflared")
	if e != nil {
		return result, &runner.Error{Code: "TUNNEL_FAILED", Err: fmt.Errorf("cloudflared not found: %w", e)}
	}
	executable, e = filepath.Abs(executable)
	if e != nil {
		return result, e
	}
	info, e := os.Stat(executable)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return result, &runner.Error{Code: "TUNNEL_FAILED", Err: errors.New("invalid cloudflared executable")}
	}
	credentials, password, e := shareauth.NewCredentials()
	if e != nil {
		return result, e
	}
	id, e := newID()
	if e != nil {
		return result, e
	}
	dir, e := m.Store.PublicationDir(id, true)
	if e != nil {
		return result, e
	}
	f, e := os.OpenFile(filepath.Join(dir, "cloudflared.yml"), os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if e != nil {
		return result, e
	}
	_, e = f.WriteString("no-autoupdate: true\n")
	if e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return result, e
	}
	owner, e := procid.Observe(os.Getpid())
	if e != nil {
		return result, e
	}
	p := state.Publication{Owner: owner, ID: id, ProjectID: plan.ProjectID, RootID: plan.Root.InstanceID, RootRunID: plan.Root.RunID, Mode: "live", Phase: "preparing", PreviousID: prior.ID, Cloudflared: executable, Salt: credentials.Salt, Verifier: credentials.Verifier, SigningKey: credentials.SigningKey, RoutedRuns: map[string]string{}}
	for _, target := range plan.Routed {
		p.RoutedRuns[target.InstanceID] = target.RunID
	}
	lock, e := m.Store.LockPublicationRegistry()
	if e != nil {
		return result, e
	}
	registry, e := m.Store.ReadPublications()
	if e == nil {
		registry.Publications[id] = p
		registry.Revision++
		e = m.Store.WritePublications(registry)
	}
	lock.Close()
	if e != nil {
		return result, e
	}
	activated := false
	defer func() {
		if retErr != nil && !activated {
			cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			retErr = errors.Join(retErr, runner.ClosePublication(cleanup, m.Store, id))
		}
	}()
	binary := m.Executable
	if binary == "" {
		binary, e = os.Executable()
		if e != nil {
			return result, e
		}
	}
	cmd := exec.Command(binary, "__rdr_publication", m.Store.Root, id)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if e = cmd.Start(); e != nil {
		return result, e
	}
	go func() { _ = cmd.Wait() }()
	wait, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var rt state.PublicationRuntime
	for {
		rt, e = m.Store.ReadPublicationRuntime(id)
		if e == nil && rt.State == "ready" {
			break
		}
		if e == nil && (rt.State == "failed" || rt.State == "stopped") {
			return result, &runner.Error{Code: "TUNNEL_FAILED", Err: errors.New("publication supervisor failed")}
		}
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return result, e
		}
		select {
		case <-wait.Done():
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			return result, &runner.Error{Code: "READINESS_TIMEOUT", Err: errors.New("tunnel readiness timeout")}
		case <-time.After(25 * time.Millisecond):
		}
	}
	if !validQuickURL(rt.URL) {
		return result, &runner.Error{Code: "TUNNEL_FAILED", Err: errors.New("invalid quick tunnel URL")}
	}
	for _, ref := range []procid.Ref{rt.Supervisor, rt.Tunnel} {
		live, err := procid.Check(ref)
		if err != nil || !live {
			return result, &runner.Error{Code: "TUNNEL_FAILED", Err: errors.New("publication process not live")}
		}
	}
	lock, e = m.Store.LockPublicationRegistry()
	if e != nil {
		return result, e
	}
	registry, e = m.Store.ReadPublications()
	if e == nil {
		current, ok := registry.Publications[id]
		if !ok || current.Phase != "preparing" {
			e = errors.New("publication changed")
		} else {
			if prior.ID != "" {
				old, exists := registry.Publications[prior.ID]
				if !exists || old.Phase != "active" {
					e = &runner.Error{Code: "STATE_CONFLICT", Err: errors.New("previous publication changed")}
				} else {
					old.Phase = "closing"
					registry.Publications[prior.ID] = old
				}
			}
			current.Phase = "active"
			registry.Publications[id] = current
			registry.Revision++
			if e == nil {
				e = m.Store.WritePublications(registry)
			}
		}
	}
	lock.Close()
	if e != nil {
		// Rename may have succeeded before a directory sync failed. Retain an
		// observed active candidate and report it instead of rolling it back.
		if observed, readErr := m.Store.ReadPublications(); readErr == nil && observed.Publications[id].Phase == "active" {
			activated = true
			result = Result{Status: publicStatus(p, rt)}
		}
		return result, e
	}
	activated = true
	result = Result{Status: publicStatus(p, rt)}
	if prior.ID != "" {
		if e = runner.ClosePublication(ctx, m.Store, prior.ID); e != nil {
			return result, e
		}
	}
	result = Result{Status: publicStatus(p, rt), Password: password}
	return result, nil
}
func (m *Manager) Unshare(ctx context.Context, selection runner.Selection) (Result, error) {
	id, e := m.validateSelection(selection)
	if e != nil {
		return Result{}, e
	}
	reg, e := m.Store.ReadPublications()
	if e != nil {
		return Result{}, e
	}
	ids := []string{}
	locksSet := map[string]bool{id: true}
	for key, p := range reg.Publications {
		if p.RootID == id && p.Phase != "closed" {
			ids = append(ids, key)
			for target := range p.RoutedRuns {
				locksSet[target] = true
			}
		}
	}
	if len(ids) == 0 {
		return Result{Status: Status{State: "stopped", Root: Target{InstanceID: id}}}, nil
	}
	lockIDs := make([]string, 0, len(locksSet))
	for key := range locksSet {
		lockIDs = append(lockIDs, key)
	}
	sort.Strings(lockIDs)
	locks, e := m.Store.LockInstances(lockIDs...)
	if e != nil {
		return Result{}, e
	}
	defer locks.Close()
	current, e := m.Store.ReadPublications()
	if e != nil {
		return Result{}, e
	}
	for key, p := range current.Publications {
		if p.RootID == id && p.Phase != "closed" {
			for target := range p.RoutedRuns {
				if !locksSet[target] {
					return Result{}, &runner.Error{Code: "STATE_CONFLICT", Err: errors.New("publication targets changed")}
				}
			}
			present := false
			for _, prior := range ids {
				if prior == key {
					present = true
				}
			}
			if !present {
				return Result{}, &runner.Error{Code: "STATE_CONFLICT", Err: errors.New("publication set changed")}
			}
		}
	}
	sort.Strings(ids)
	for _, publicationID := range ids {
		if e = runner.ClosePublication(ctx, m.Store, publicationID); e != nil {
			return Result{}, e
		}
	}
	return Result{Status: Status{State: "stopped", Root: Target{InstanceID: id}}}, nil
}
