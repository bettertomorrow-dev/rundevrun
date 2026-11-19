// Builds publication artifacts using service environments and verified dependencies.
package runner

import (
	"context"
	"fmt"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
	"strings"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/config"
	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
)

func SnapshotEnvironment(service config.Service, values map[string]string, dependencies map[string]map[string]string, project config.Project) (string, []string, error) {
	for dep, addresses := range dependencies {
		for key, value := range addresses {
			values["dependencies."+dep+"."+key] = value
		}
	}
	env, err := environment(service, values, project)
	if err != nil {
		return "", nil, err
	}
	if overrides, ok := service.Share["env"].(map[string]any); ok {
		for key, value := range overrides {
			raw, ok := value.(string)
			if !ok {
				return "", nil, fmt.Errorf("snapshot environment %s is not a string", key)
			}
			expanded, _, e := substitute(raw, values, false)
			if e != nil {
				return "", nil, e
			}
			env[key] = expanded
		}
	}
	for dep, addresses := range dependencies {
		prefix := "RDR_DEP_" + strings.ToUpper(strings.ReplaceAll(dep, "-", "_")) + "_"
		for key, value := range addresses {
			env[prefix+strings.ToUpper(key)] = value
		}
	}
	build, _ := service.Share["build"].(string)
	command, subs, err := substitute(build, values, true)
	if err != nil {
		return "", nil, err
	}
	return command, mergeEnv(env, subs), nil
}

func RunPublicationBuild(ctx context.Context, command, cwd string, env []string, logPath string, timeout time.Duration, binary string, persist func(procid.Ref) error, release func() error, persistMembers func([]procid.Ref) error) error {
	log, err := openLog(logPath)
	if err != nil {
		return err
	}
	defer log.Close()
	return runBuildCommand(ctx, command, cwd, env, log, timeout, binary, persist, release, persistMembers)
}

func VerifyPublicationDependency(store *state.Store, projectID, id, runID string) error {
	reg, err := store.Read()
	if err != nil {
		return err
	}
	project, err := reg.Project(projectID)
	if err != nil {
		return err
	}
	instance, ok := project.Instances[id]
	if !ok || runID == "" || instance.RunID != runID {
		return &Error{Code: "STATE_CONFLICT", Err: fmt.Errorf("snapshot dependency %s changed", id)}
	}
	if err = New(store, "").dependencyReady(id); err != nil {
		return err
	}
	manifest, err := store.ReadManifest(runID)
	if err != nil {
		return err
	}
	if manifest.InstanceID != id {
		return &Error{Code: "STATE_CORRUPT", Err: fmt.Errorf("snapshot dependency manifest mismatch")}
	}
	if manifest.Launch.Protocol == "http" {
		_, err = VerifyProxyTarget(store, projectID, id, runID)
	}
	return err
}
