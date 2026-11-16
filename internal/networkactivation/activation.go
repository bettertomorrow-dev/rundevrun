// Package networkactivation commits local route state and activates its proxy.
package networkactivation

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/bettertomorrow-dev/rundevrun/internal/localrouting"
	"github.com/bettertomorrow-dev/rundevrun/internal/networking"
	"github.com/bettertomorrow-dev/rundevrun/internal/proxy"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

type Activation struct {
	store      *state.Store
	exe        string
	path       string
	routes     runner.LocalRoutePlan
	proxyReady bool
	prepared   bool
	finished   bool
	config     networking.Config
}

func (a *Activation) RouteRevision() uint64 { return a.routes.Revision }
func (a *Activation) RouteChanges() []runner.LocalRouteChange {
	return append([]runner.LocalRouteChange(nil), a.routes.Changes...)
}

func Prepare(ctx context.Context, store *state.Store, exe, configPath string, desired networking.Config) (*Activation, error) {
	cfg, err := localrouting.Load(configPath)
	if err != nil {
		return nil, err
	}
	cfg = localrouting.WithDesired(cfg, desired)
	routes, err := runner.PlanLocalRoutes(ctx, store, cfg)
	if err != nil {
		return nil, err
	}
	a := &Activation{store: store, exe: exe, path: configPath, routes: routes, config: desired}
	info, err := proxy.Status(store)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return a, nil
	}
	a.proxyReady = info.State == "ready"
	if a.proxyReady {
		if err := proxy.PrepareReloadTo(ctx, store, desired.Local.Listen); err != nil {
			return nil, err
		}
		a.prepared = true
	}
	return a, nil
}

func (a *Activation) Commit(ctx context.Context) error {
	if a.finished {
		return errors.New("network activation already finished")
	}
	return runner.CommitLocalRoutes(ctx, a.store, a.routes)
}

func (a *Activation) Finish(ctx context.Context) error {
	if a.finished {
		return errors.New("network activation already finished")
	}
	succeeded := false
	defer func() {
		if !succeeded && a.prepared {
			_ = proxy.AbortReload(context.Background(), a.store)
		}
		a.finished = true
	}()
	if a.proxyReady {
		if !a.prepared {
			return errors.New("network activation has no listener reservation")
		}
		if err := proxy.Reload(ctx, a.store); err != nil {
			return err
		}
	} else if hasLiveHTTP(a.store) {
		if err := proxy.Ensure(ctx, a.store, a.exe, a.path); err != nil {
			return err
		}
	} else {
		succeeded = true
		return nil
	}
	info, err := proxy.Status(a.store)
	if err != nil {
		return err
	}
	if tailnetEnabled(a.config) {
		if info.TLSError != "" {
			return fmt.Errorf("managed tailnet TLS did not activate: %s", info.TLSError)
		}
		if info.TLSMode != "managed" || info.HTTPSListen == "" {
			return errors.New("managed tailnet TLS listener is not active")
		}
	}
	succeeded = true
	return nil
}

func (a *Activation) Abort() {
	if a != nil && a.prepared && !a.finished {
		_ = proxy.AbortReload(context.Background(), a.store)
		a.finished = true
	}
}

func tailnetEnabled(cfg networking.Config) bool {
	if cfg.Tailnet.Domain == "" {
		return false
	}
	for _, project := range cfg.Projects {
		if project.Tailnet {
			return true
		}
	}
	return false
}

func hasLiveHTTP(store *state.Store) bool {
	reg, err := store.Read()
	if err != nil {
		return false
	}
	for id := range reg.Projects {
		project, err := reg.Project(id)
		if err != nil {
			continue
		}
		for _, instance := range project.Instances {
			if instance.RunID == "" || !(instance.State == "starting" || instance.State == "running" || instance.State == "ready" || instance.State == "unhealthy") {
				continue
			}
			manifest, err := store.ReadManifest(instance.RunID)
			if err == nil && manifest.Launch.Protocol == "http" {
				return true
			}
		}
	}
	return false
}
