// Plans and updates service aliases.
package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

type AliasChange struct {
	InstanceID      string `json:"instance_id"`
	RunID           string `json:"run_id,omitempty"`
	OldHostname     string `json:"old_hostname,omitempty"`
	NewHostname     string `json:"new_hostname,omitempty"`
	OldURL          string `json:"old_url,omitempty"`
	NewURL          string `json:"new_url,omitempty"`
	RestartRequired bool   `json:"restart_required"`
}
type AliasPlan struct {
	ProjectID    string        `json:"project_id"`
	WorktreeID   string        `json:"worktree_id"`
	Revision     uint64        `json:"revision"`
	ConfigDigest string        `json:"config_digest"`
	OldAlias     string        `json:"old_alias,omitempty"`
	NewAlias     string        `json:"new_alias"`
	Changes      []AliasChange `json:"changes"`
	locks        []string
}

func validAlias(value string) (string, error) {
	value = strings.ToLower(value)
	if value == "" || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return "", &Error{"CONFIG_INVALID", errors.New("alias must be a DNS label of 1 to 63 characters")}
	}
	for _, ch := range value {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
			return "", &Error{"CONFIG_INVALID", errors.New("alias must contain only letters, digits and hyphens")}
		}
	}
	return value, nil
}

func (r *Runner) PlanAlias(ctx context.Context, project *discovery.Project, worktree *discovery.Worktree, label string) (AliasPlan, error) {
	if err := ctx.Err(); err != nil {
		return AliasPlan{}, &Error{"CANCELLED", err}
	}
	if r == nil || r.Store == nil || project == nil || worktree == nil {
		return AliasPlan{}, &Error{"USAGE", errors.New("missing alias selection")}
	}
	reg, err := r.Store.Read()
	if err != nil {
		return AliasPlan{}, err
	}
	return r.planAlias(ctx, reg, project, worktree, label)
}

func (r *Runner) planAlias(ctx context.Context, reg state.Registry, project *discovery.Project, worktree *discovery.Worktree, label string) (AliasPlan, error) {
	if err := ctx.Err(); err != nil {
		return AliasPlan{}, &Error{"CANCELLED", err}
	}
	label, err := validAlias(label)
	if err != nil {
		return AliasPlan{}, err
	}
	if r.Routing == nil || !r.Routing.Routing.Enabled {
		return AliasPlan{}, &Error{"CONFIG_INVALID", errors.New("enable routing before assigning an alias")}
	}
	saved, err := reg.Project(project.ID)
	if err != nil {
		return AliasPlan{}, err
	}
	wi := -1
	for i, w := range saved.Worktrees {
		if w.ID == worktree.ID && w.Root == worktree.Root {
			wi = i
			break
		}
	}
	if wi < 0 {
		return AliasPlan{}, &Error{"NOT_FOUND", errors.New("worktree is not registered")}
	}
	cfg, err := discovery.LoadConfig(saved.Worktrees[wi].Root)
	if err != nil {
		return AliasPlan{}, &Error{"CONFIG_INVALID", fmt.Errorf("worktree configuration: %w", err)}
	}
	if cfg.Project.Name != saved.Name {
		return AliasPlan{}, &Error{"CONFIG_INVALID", errors.New("worktree project name changed")}
	}
	plan := AliasPlan{ProjectID: project.ID, WorktreeID: worktree.ID, Revision: reg.Revision, ConfigDigest: cfg.Digest, OldAlias: saved.Worktrees[wi].Alias, NewAlias: label, Changes: []AliasChange{}}
	oldAlias := plan.OldAlias
	if oldAlias == "" {
		oldAlias = filepath.Base(saved.Worktrees[wi].Root)
	}
	aliasChanged := !strings.EqualFold(oldAlias, label)
	owners := map[string]string{}
	for pid := range reg.Projects {
		p, e := reg.Project(pid)
		if e != nil {
			return AliasPlan{}, e
		}
		for _, inst := range p.Instances {
			for _, ep := range inst.Endpoints {
				host := strings.ToLower(ep.Hostname)
				if other := owners[host]; other != "" && other != inst.ID {
					return AliasPlan{}, &Error{"ROUTE_CONFLICT", fmt.Errorf("hostname %s is already reserved", host)}
				}
				owners[host] = inst.ID
			}
			if inst.Hostname != "" && !(r.Routing.LocalOnly && r.Routing.Legacy != nil && !strings.HasSuffix(inst.Hostname, ".localhost")) {
				host := strings.ToLower(inst.Hostname)
				if other := owners[host]; other != "" && other != inst.ID {
					return AliasPlan{}, &Error{"ROUTE_CONFLICT", fmt.Errorf("hostname %s is already reserved", host)}
				}
				owners[host] = inst.ID
			}
		}
	}
	ids := make([]string, 0)
	for id, inst := range saved.Instances {
		if inst.WorktreeID == worktree.ID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	plan.locks = append(plan.locks, ids...)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return AliasPlan{}, &Error{"CANCELLED", err}
		}
		inst := saved.Instances[id]
		if inst.Preparation != nil {
			return AliasPlan{}, &Error{"BUSY", fmt.Errorf("instance %s has pending preparation", id)}
		}
		observed, e := r.Store.Inspect(inst)
		if e != nil {
			return AliasPlan{}, &Error{"OWNERSHIP_UNKNOWN", e}
		}
		live := liveState(observed)
		if live && inst.RunID == "" {
			return AliasPlan{}, &Error{"OWNERSHIP_UNKNOWN", errors.New("live instance has no run identity")}
		}
		isHTTP := false
		var manifest state.Manifest
		if live && inst.RunID != "" {
			manifest, e = r.Store.ReadManifest(inst.RunID)
			if e != nil {
				return AliasPlan{}, e
			}
			if manifest.InstanceID != id {
				return AliasPlan{}, fmt.Errorf("%w: manifest instance mismatch", state.ErrCorrupt)
			}
			isHTTP = manifest.Launch.Protocol == "http"
		} else {
			svc, ok := cfg.Services[inst.Service]
			if !ok {
				if inst.Hostname != "" {
					return AliasPlan{}, &Error{"CONFIG_INVALID", fmt.Errorf("service %s is missing from configuration", inst.Service)}
				}
				continue
			}
			isHTTP = svc.Protocol == "http"
		}
		if !isHTTP {
			continue
		}
		host := inst.Hostname
		if r.Routing.LocalOnly {
			for _, ep := range inst.Endpoints {
				if ep.Scope == "local" && !ep.Alias {
					host = ep.Hostname
					break
				}
			}
		}
		if aliasChanged || host == "" {
			domain := ""
			if !r.Routing.LocalOnly && cfg.Routing != nil {
				domain = cfg.Routing.Domain
			}
			host, e = r.Routing.Hostname(inst.Service, label, saved.Name, domain)
			if e != nil {
				return AliasPlan{}, &Error{"CONFIG_INVALID", e}
			}
		}
		url, e := r.Routing.URL(host)
		if e != nil {
			return AliasPlan{}, &Error{"CONFIG_INVALID", e}
		}
		c := AliasChange{InstanceID: id, RunID: inst.RunID, OldHostname: inst.Hostname, NewHostname: host, OldURL: inst.RouteURL, NewURL: url}
		if live {
			c.RestartRequired = manifest.Addresses["url"] != url
		}
		plan.Changes = append(plan.Changes, c)
	}
	for _, c := range plan.Changes {
		if r.Routing.LocalOnly && r.Routing.TailnetDomain != "" && r.Routing.LocalProjects[project.ID].Tailnet {
			tailHost := strings.TrimSuffix(c.NewHostname, ".localhost") + "." + r.Routing.TailnetDomain
			if owner := owners[tailHost]; owner != "" && owner != c.InstanceID {
				return AliasPlan{}, &Error{"ROUTE_CONFLICT", fmt.Errorf("hostname %s is already reserved", tailHost)}
			}
		}
		if owner := owners[strings.ToLower(c.NewHostname)]; owner != "" && owner != c.InstanceID {
			return AliasPlan{}, &Error{"ROUTE_CONFLICT", fmt.Errorf("hostname %s is already reserved; choose another alias", c.NewHostname)}
		}
	}

	for _, c := range plan.Changes {
		if c.OldHostname != "" && !strings.EqualFold(c.OldHostname, c.NewHostname) {
			delete(owners, strings.ToLower(c.OldHostname))
		}
	}
	for _, c := range plan.Changes {
		host := strings.ToLower(c.NewHostname)
		if owner := owners[host]; owner != "" && owner != c.InstanceID {
			return AliasPlan{}, &Error{"ROUTE_CONFLICT", fmt.Errorf("hostname %s is already reserved; choose another alias", host)}
		}
		owners[host] = c.InstanceID
	}
	hosts := make([]string, 0, len(owners))
	for host := range owners {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	if len(hosts) > 0 {
		if err = routing.ValidateHosts(*r.Routing, hosts); err != nil {
			var re *routing.Error
			if errors.As(err, &re) {
				return AliasPlan{}, &Error{re.Code, re.Err}
			}
			return AliasPlan{}, err
		}
	}
	return plan, nil
}

func (r *Runner) SetAlias(ctx context.Context, project *discovery.Project, worktree *discovery.Worktree, label string) (AliasPlan, error) {
	initial, err := r.PlanAlias(ctx, project, worktree, label)
	if err != nil {
		return AliasPlan{}, err
	}
	locks, err := r.Store.LockInstances(initial.locks...)
	if err != nil {
		if errors.Is(err, state.ErrBusy) {
			return AliasPlan{}, &Error{"BUSY", err}
		}
		return AliasPlan{}, err
	}
	defer locks.Close()
	next, err := r.PlanAlias(ctx, project, worktree, label)
	if err != nil {
		return AliasPlan{}, err
	}
	if !reflect.DeepEqual(initial.locks, next.locks) {
		return AliasPlan{}, &Error{"STATE_CONFLICT", errors.New("worktree instances changed while locking")}
	}
	err = r.Store.Update(func(reg *state.Registry) error {
		if reg.Revision != next.Revision {
			return &Error{"STATE_CONFLICT", errors.New("registry changed while assigning alias")}
		}
		current, e := r.planAlias(ctx, *reg, project, worktree, label)
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(current, next) {
			return &Error{"STATE_CONFLICT", errors.New("alias candidate changed before commit")}
		}
		saved, e := reg.Project(project.ID)
		if e != nil {
			return e
		}
		for i := range saved.Worktrees {
			if saved.Worktrees[i].ID == worktree.ID {
				saved.Worktrees[i].Alias = next.NewAlias
			}
		}
		for _, c := range next.Changes {
			inst := saved.Instances[c.InstanceID]
			r.setCanonicalEndpoint(&inst, project.ID, c.NewHostname, c.NewURL)
			saved.Instances[c.InstanceID] = inst
		}
		return reg.SetProject(saved.ID, saved)
	})
	if err != nil {
		return AliasPlan{}, err
	}
	return next, nil
}
