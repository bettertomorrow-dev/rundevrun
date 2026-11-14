package publication

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

// SnapshotPlan describes the pinned inputs without exposing build environment values.
type SnapshotPlan struct {
	LivePlan
	Digest       string `json:"digest"`
	CWD          string `json:"cwd"`
	ArtifactDir  string `json:"artifact_dir"`
	service      config.Service
	projectName  string
	dependencies map[string]string
	buildRuns    map[string]string
}

// PlanSnapshot checks the active graph and captures the fresh snapshot configuration.
// It does not create publication state or execute the build.
func (m *Manager) PlanSnapshot(ctx context.Context, selection runner.Selection) (SnapshotPlan, error) {
	live, err := m.PlanLive(ctx, selection)
	if err != nil {
		return SnapshotPlan{}, err
	}
	cfg, err := config.Load(filepath.Join(selection.Worktree.Root, "dev.toml"))
	if err != nil {
		return SnapshotPlan{}, err
	}
	service, ok := cfg.Services[selection.Service]
	if !ok {
		return SnapshotPlan{}, &runner.Error{Code: "CONFIG_INVALID", Err: fmt.Errorf("service %q is not configured", selection.Service)}
	}
	mode, _ := service.Share["mode"].(string)
	build, _ := service.Share["build"].(string)
	dir, _ := service.Share["dir"].(string)
	if mode != "snapshot" || build == "" || dir == "" {
		return SnapshotPlan{}, &runner.Error{Code: "CONFIG_INVALID", Err: fmt.Errorf("snapshot build and directory are not configured")}
	}
	reg, err := m.Store.Read()
	if err != nil {
		return SnapshotPlan{}, err
	}
	if reg.Revision != live.Revision {
		return SnapshotPlan{}, &runner.Error{Code: "STATE_CONFLICT", Err: fmt.Errorf("registry changed during snapshot planning")}
	}
	project, err := reg.Project(live.ProjectID)
	if err != nil {
		return SnapshotPlan{}, err
	}
	if cfg.Project.Name != project.Name {
		return SnapshotPlan{}, &runner.Error{Code: "CONFIG_INVALID", Err: fmt.Errorf("project name changed")}
	}
	instance := project.Instances[live.Root.InstanceID]
	manifest, err := m.Store.ReadManifest(live.Root.RunID)
	if err != nil {
		return SnapshotPlan{}, err
	}
	routes := make([]state.Route, 0, len(service.Routes))
	seenRoutes := map[string]bool{}
	for _, route := range service.Routes {
		if seenRoutes[route.Dependency] {
			return SnapshotPlan{}, &runner.Error{Code: "CONFIG_INVALID", Err: fmt.Errorf("multiple public routes for dependency %q", route.Dependency)}
		}
		seenRoutes[route.Dependency] = true
		routes = append(routes, state.Route{Path: route.Path, Dependency: route.Dependency, StripPrefix: route.StripPrefix})
	}
	if len(routes) != len(manifest.Routes) || (len(routes) > 0 && !reflect.DeepEqual(routes, manifest.Routes)) {
		return SnapshotPlan{}, &runner.Error{Code: "BINDING_INVALID", Err: fmt.Errorf("configured routes differ from active run")}
	}
	if len(service.DependsOn) != len(manifest.EffectiveBindings) {
		return SnapshotPlan{}, &runner.Error{Code: "BINDING_INVALID", Err: fmt.Errorf("configured dependencies differ from active run")}
	}
	declared := map[string]bool{}
	for _, dep := range service.DependsOn {
		declared[dep] = true
	}
	for dep := range instance.DesiredBindings {
		if !declared[dep] {
			return SnapshotPlan{}, &runner.Error{Code: "BINDING_INVALID", Err: fmt.Errorf("undeclared desired dependency %s", dep)}
		}
	}
	for _, dep := range service.DependsOn {
		desired := instance.DesiredBindings[dep]
		if desired == "" {
			desired = live.ProjectID + "/" + selection.Worktree.ID + "/" + dep
		}
		if desired != manifest.EffectiveBindings[dep] {
			return SnapshotPlan{}, &runner.Error{Code: "BINDING_INVALID", Err: fmt.Errorf("dependency %q has pending binding", dep)}
		}
	}
	dependencies := make(map[string]string, len(manifest.EffectiveBindings))
	buildRuns := make(map[string]string, len(manifest.EffectiveBindings))
	for dep, id := range manifest.EffectiveBindings {
		parts := strings.Split(id, "/")
		if len(parts) != 3 || parts[0] != project.ID || parts[2] != dep {
			return SnapshotPlan{}, &runner.Error{Code: "BINDING_INVALID", Err: fmt.Errorf("invalid snapshot dependency %s", dep)}
		}
		instance, ok := project.Instances[id]
		if !ok || instance.RunID == "" {
			return SnapshotPlan{}, &runner.Error{Code: "BINDING_INVALID", Err: fmt.Errorf("snapshot dependency %s is not running", dep)}
		}
		if err := runner.VerifyPublicationDependency(m.Store, project.ID, id, instance.RunID); err != nil {
			return SnapshotPlan{}, err
		}
		dependencies[dep] = instance.RunID
		buildRuns[id] = instance.RunID
	}
	values := map[string]string{"worktree": selection.Worktree.Root, "project": project.Name, "service": selection.Service, "run_dir": filepath.Join(m.Store.Root, "publications", "<publication>")}
	for key, value := range manifest.Addresses {
		values[key] = value
	}
	values["url"] = "https://snapshot.invalid"
	addresses := map[string]map[string]string{}
	for dep, runID := range dependencies {
		target, err := m.Store.ReadManifest(runID)
		if err != nil {
			return SnapshotPlan{}, err
		}
		fields := map[string]string{}
		for key, value := range target.Addresses {
			if key != "url" {
				fields[key] = value
			}
		}
		for _, route := range manifest.Routes {
			if route.Dependency == dep {
				fields["url"] = "https://snapshot.invalid" + route.Path
				break
			}
		}
		addresses[dep] = fields
	}
	if _, _, err := runner.SnapshotEnvironment(service, values, addresses); err != nil {
		return SnapshotPlan{}, &runner.Error{Code: "CONFIG_INVALID", Err: err}
	}
	locks := map[string]bool{}
	for _, id := range live.Locks {
		locks[id] = true
	}
	for _, id := range manifest.EffectiveBindings {
		locks[id] = true
	}
	live.Locks = live.Locks[:0]
	for id := range locks {
		live.Locks = append(live.Locks, id)
	}
	sort.Strings(live.Locks)
	cwd, err := filepath.EvalSymlinks(filepath.Join(selection.Worktree.Root, service.CWD))
	if err != nil {
		return SnapshotPlan{}, err
	}
	if artifact, e := filepath.EvalSymlinks(filepath.Join(cwd, dir)); e == nil {
		rel, e := filepath.Rel(cwd, artifact)
		if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return SnapshotPlan{}, &runner.Error{Code: "CONFIG_INVALID", Err: fmt.Errorf("snapshot artifact escapes cwd")}
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return SnapshotPlan{}, e
	}
	return SnapshotPlan{LivePlan: live, Digest: cfg.Digest, CWD: cwd, ArtifactDir: dir, service: service, projectName: project.Name, dependencies: dependencies, buildRuns: buildRuns}, nil
}
