// Plans and reserves explicit service hostnames.
package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

type HostnameAliasPlan struct {
	ProjectID       string `json:"project_id"`
	WorktreeID      string `json:"worktree_id"`
	InstanceID      string `json:"instance_id"`
	Revision        uint64 `json:"revision"`
	ConfigDigest    string `json:"config_digest"`
	OldHostname     string `json:"old_hostname,omitempty"`
	NewHostname     string `json:"new_hostname"`
	OldURL          string `json:"old_url,omitempty"`
	NewURL          string `json:"new_url"`
	RunID           string `json:"run_id,omitempty"`
	RestartRequired bool   `json:"restart_required"`
}

func (r *Runner) PlanHostnameAlias(ctx context.Context, selection Selection, hostname string) (HostnameAliasPlan, error) {
	if err := ctx.Err(); err != nil {
		return HostnameAliasPlan{}, &Error{"CANCELLED", err}
	}
	if r == nil || r.Store == nil {
		return HostnameAliasPlan{}, &Error{"USAGE", errors.New("missing runner")}
	}
	if _, err := selectionID(selection); err != nil {
		return HostnameAliasPlan{}, err
	}
	reg, err := r.Store.Read()
	if err != nil {
		return HostnameAliasPlan{}, err
	}
	return r.planHostnameAlias(ctx, reg, selection, hostname)
}
func (r *Runner) planHostnameAlias(ctx context.Context, reg state.Registry, selection Selection, hostname string) (HostnameAliasPlan, error) {
	if err := ctx.Err(); err != nil {
		return HostnameAliasPlan{}, &Error{"CANCELLED", err}
	}
	id, err := selectionID(selection)
	if err != nil {
		return HostnameAliasPlan{}, err
	}
	if r.Routing == nil || !r.Routing.Routing.Enabled {
		return HostnameAliasPlan{}, &Error{"CONFIG_INVALID", errors.New("enable routing before assigning a hostname")}
	}
	project, err := reg.Project(selection.Project.ID)
	if err != nil {
		return HostnameAliasPlan{}, err
	}
	inst, ok := project.Instances[id]
	if !ok {
		return HostnameAliasPlan{}, &Error{"NOT_FOUND", errors.New("instance is not registered")}
	}
	var root string
	for _, w := range project.Worktrees {
		if w.ID == selection.Worktree.ID && w.Root == selection.Worktree.Root {
			root = w.Root
			break
		}
	}
	if root == "" {
		return HostnameAliasPlan{}, &Error{"NOT_FOUND", errors.New("worktree is not registered")}
	}
	cfg, err := config.Load(filepath.Join(root, "dev.toml"))
	if err != nil {
		return HostnameAliasPlan{}, &Error{"CONFIG_INVALID", fmt.Errorf("worktree configuration: %w", err)}
	}
	if cfg.Project.Name != project.Name {
		return HostnameAliasPlan{}, &Error{"CONFIG_INVALID", errors.New("worktree project name changed")}
	}
	if inst.Preparation != nil {
		return HostnameAliasPlan{}, &Error{"BUSY", errors.New("instance has a pending preparation")}
	}
	observed, err := r.Store.Inspect(inst)
	if err != nil {
		return HostnameAliasPlan{}, &Error{"OWNERSHIP_UNKNOWN", err}
	}
	live := liveState(observed)
	if live && inst.RunID == "" {
		return HostnameAliasPlan{}, &Error{"OWNERSHIP_UNKNOWN", errors.New("live instance has no run identity")}
	}
	var manifest state.Manifest
	isHTTP := false
	if live {
		manifest, err = r.Store.ReadManifest(inst.RunID)
		if err != nil {
			return HostnameAliasPlan{}, err
		}
		if manifest.InstanceID != id {
			return HostnameAliasPlan{}, fmt.Errorf("%w: manifest instance mismatch", state.ErrCorrupt)
		}
		isHTTP = manifest.Launch.Protocol == "http"
	} else {
		svc, ok := cfg.Services[inst.Service]
		if !ok {
			return HostnameAliasPlan{}, &Error{"CONFIG_INVALID", errors.New("service is missing from configuration")}
		}
		isHTTP = svc.Protocol == "http"
	}
	if !isHTTP {
		return HostnameAliasPlan{}, &Error{"USAGE", errors.New("hostname aliases require an HTTP service")}
	}
	hostname = strings.ToLower(hostname)
	url, err := r.Routing.URL(hostname)
	if err != nil {
		return HostnameAliasPlan{}, &Error{"CONFIG_INVALID", err}
	}
	domain := r.Routing.Routing.Domain
	if cfg.Routing != nil {
		domain = cfg.Routing.Domain
	}
	domain = strings.ToLower(domain)
	suffix := "." + domain
	if domain == "" || !strings.HasSuffix(hostname, suffix) || strings.Contains(strings.TrimSuffix(hostname, suffix), ".") || strings.TrimSuffix(hostname, suffix) == "" {
		return HostnameAliasPlan{}, &Error{"CONFIG_INVALID", errors.New("hostname must be one label under the configured domain")}
	}
	owners := map[string]string{}
	for pid := range reg.Projects {
		p, e := reg.Project(pid)
		if e != nil {
			return HostnameAliasPlan{}, e
		}
		for _, existing := range p.Instances {
			if existing.Hostname != "" {
				host := strings.ToLower(existing.Hostname)
				if owner := owners[host]; owner != "" && owner != existing.ID {
					return HostnameAliasPlan{}, &Error{"ROUTE_CONFLICT", fmt.Errorf("hostname %s is already reserved", host)}
				}
				owners[host] = existing.ID
			}
		}
	}
	if owner := owners[hostname]; owner != "" && owner != id {
		return HostnameAliasPlan{}, &Error{"ROUTE_CONFLICT", fmt.Errorf("hostname %s is already reserved", hostname)}
	}
	if inst.Hostname != "" && !strings.EqualFold(inst.Hostname, hostname) {
		delete(owners, strings.ToLower(inst.Hostname))
	}
	owners[hostname] = id
	hosts := make([]string, 0, len(owners))
	for host := range owners {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	if err = routing.ValidateHosts(*r.Routing, hosts); err != nil {
		var re *routing.Error
		if errors.As(err, &re) {
			return HostnameAliasPlan{}, &Error{re.Code, re.Err}
		}
		return HostnameAliasPlan{}, err
	}
	if err := ctx.Err(); err != nil {
		return HostnameAliasPlan{}, &Error{"CANCELLED", err}
	}
	plan := HostnameAliasPlan{ProjectID: project.ID, WorktreeID: selection.Worktree.ID, InstanceID: id, Revision: reg.Revision, ConfigDigest: cfg.Digest, OldHostname: inst.Hostname, NewHostname: hostname, OldURL: inst.RouteURL, NewURL: url, RunID: inst.RunID}
	if live {
		plan.RestartRequired = manifest.Addresses["url"] != url
	}
	return plan, nil
}

func (r *Runner) SetHostnameAlias(ctx context.Context, selection Selection, hostname string) (HostnameAliasPlan, error) {
	initial, err := r.PlanHostnameAlias(ctx, selection, hostname)
	if err != nil {
		return HostnameAliasPlan{}, err
	}
	locks, err := r.Store.LockInstances(initial.InstanceID)
	if err != nil {
		if errors.Is(err, state.ErrBusy) {
			return HostnameAliasPlan{}, &Error{"BUSY", err}
		}
		return HostnameAliasPlan{}, err
	}
	defer locks.Close()
	next, err := r.PlanHostnameAlias(ctx, selection, hostname)
	if err != nil {
		return HostnameAliasPlan{}, err
	}
	err = r.Store.Update(func(reg *state.Registry) error {
		if reg.Revision != next.Revision {
			return &Error{"STATE_CONFLICT", errors.New("registry changed while assigning hostname")}
		}
		current, e := r.planHostnameAlias(ctx, *reg, selection, hostname)
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(current, next) {
			return &Error{"STATE_CONFLICT", errors.New("hostname candidate changed before commit")}
		}
		project, e := reg.Project(next.ProjectID)
		if e != nil {
			return e
		}
		inst := project.Instances[next.InstanceID]
		inst.Hostname = next.NewHostname
		inst.RouteURL = next.NewURL
		project.Instances[inst.ID] = inst
		return reg.SetProject(project.ID, project)
	})
	if err != nil {
		return HostnameAliasPlan{}, err
	}
	return next, nil
}
