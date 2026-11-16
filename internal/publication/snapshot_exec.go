// Builds and publishes static snapshots.
package publication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/snapshot"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
	"golang.org/x/sys/unix"
)

var errSnapshotTunnelLost = errors.New("tunnel failed during snapshot build")

type snapshotBuildSpec struct {
	Digest, Worktree, Project, Service, CWD, Artifact string
	Config                                            config.Service
	Dependencies                                      map[string]string
}

func writeSnapshotSpec(dir string, spec snapshotBuildSpec) error {
	f, err := os.OpenFile(filepath.Join(dir, "snapshot.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(spec)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

func buildSnapshot(ctx context.Context, store *state.Store, p state.Publication, record *state.PublicationRuntime, public, dir string, lease func() (bool, error), tunnelDone <-chan error) (result error) {
	defer func() {
		if result == nil {
			return
		}
		log, err := runner.OpenProxyLog(filepath.Join(dir, "build.log"))
		if err != nil {
			result = errors.Join(result, err)
			return
		}
		_, err = fmt.Fprintln(log, "rdr: snapshot preparation failed:", result)
		result = errors.Join(result, err, log.Close())
	}()
	f, err := os.Open(filepath.Join(dir, "snapshot.json"))
	if err != nil {
		return err
	}
	var spec snapshotBuildSpec
	dec := json.NewDecoder(io.LimitReader(f, 1<<20))
	dec.DisallowUnknownFields()
	err = dec.Decode(&spec)
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	cfg, err := config.Load(filepath.Join(spec.Worktree, "dev.toml"))
	if err != nil {
		return err
	}
	if cfg.Digest != spec.Digest {
		return errors.New("snapshot configuration changed before build")
	}
	manifest, err := store.ReadManifest(p.RootRunID)
	if err != nil {
		return err
	}
	values := map[string]string{"worktree": spec.Worktree, "project": spec.Project, "service": spec.Service, "run_dir": dir}
	for key, value := range manifest.Addresses {
		values[key] = value
	}
	values["url"] = public
	deps := make(map[string]map[string]string)
	for dep, id := range manifest.EffectiveBindings {
		runID := spec.Dependencies[dep]
		if err := runner.VerifyPublicationDependency(store, p.ProjectID, id, runID); err != nil {
			return err
		}
		target, e := store.ReadManifest(runID)
		if e != nil {
			return e
		}
		addresses := map[string]string{}
		for key, value := range target.Addresses {
			if key != "url" {
				addresses[key] = value
			}
		}
		for _, route := range manifest.Routes {
			if route.Dependency == dep {
				addresses["url"] = public + strings.TrimSuffix(route.Path, "/")
				break
			}
		}
		deps[dep] = addresses
	}
	command, env, err := runner.SnapshotEnvironment(spec.Config, values, deps)
	if err != nil {
		return err
	}
	timeout, err := time.ParseDuration(spec.Config.BuildTimeout)
	if err != nil {
		return err
	}
	buildCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	gate, err := store.LockPublicationGate(p.ID)
	if err != nil {
		return err
	}
	reg, err := store.ReadPublications()
	if err != nil {
		gate.Close()
		return err
	}
	current, ok := reg.Publications[p.ID]
	if !ok || current.Phase != "preparing" {
		gate.Close()
		return errors.New("snapshot authorization revoked")
	}
	binary, err := os.Executable()
	if err != nil {
		gate.Close()
		return err
	}
	leaseCtx, cancelLease := context.WithCancel(buildCtx)
	defer cancelLease()
	leaseDone := make(chan error, 1)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case tunnelErr := <-tunnelDone:
				if tunnelErr == nil {
					tunnelErr = errors.New("tunnel exited during snapshot build")
				}
				leaseDone <- errors.Join(errSnapshotTunnelLost, tunnelErr)
				cancelLease()
				return
			case <-tick.C:
				valid, err := lease()
				if err != nil || !valid {
					if err == nil {
						err = errors.New("snapshot authorization revoked")
					}
					leaseDone <- err
					cancelLease()
					return
				}
			}
		}
	}()
	gateOpen := true
	defer func() {
		if gateOpen {
			_ = gate.Close()
		}
	}()
	err = runner.RunPublicationBuild(leaseCtx, command, spec.CWD, env, filepath.Join(dir, "build.log"), timeout, binary,
		func(ref procid.Ref) error { record.Build = ref; return store.WritePublicationRuntime(p.ID, *record) },
		func() error { gateOpen = false; return gate.Close() },
		func(refs []procid.Ref) error {
			record.BuildMembers = refs
			return store.WritePublicationRuntime(p.ID, *record)
		})
	if err == nil {
		err = snapshot.Copy(leaseCtx, spec.CWD, spec.Artifact, filepath.Join(dir, "snapshot"))
	} else {
		err = fmt.Errorf("snapshot build failed: %w", err)
	}
	cancelLease()
	<-monitorDone
	select {
	case leaseErr := <-leaseDone:
		return errors.Join(err, leaseErr)
	default:
		return err
	}
}
