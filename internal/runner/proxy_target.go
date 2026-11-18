// Verifies service processes and listeners before proxy connections.
package runner

import (
	"errors"
	"fmt"
	"io"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func VerifyProxyTarget(store *state.Store, projectID, instanceID, expectedRunID string) (int, error) {
	if expectedRunID == "" {
		return 0, errors.New("no active run")
	}
	checkRegistry := func() error {
		reg, err := store.Read()
		if err != nil {
			return err
		}
		project, err := reg.Project(projectID)
		if err != nil {
			return err
		}
		inst, ok := project.Instances[instanceID]
		if !ok || inst.RunID != expectedRunID || inst.State == "stopped" || inst.State == "failed" || inst.Intent != nil && (inst.Intent.Phase == "stopping" || inst.Intent.Phase == "stopped" || inst.Intent.Phase == "interrupted") {
			return errors.New("instance no longer active")
		}
		return nil
	}
	if err := checkRegistry(); err != nil {
		return 0, err
	}
	m, err := store.ReadManifest(expectedRunID)
	if err != nil {
		return 0, err
	}
	if m.InstanceID != instanceID || m.Launch.Protocol != "http" || m.Launch.Port < 1 || m.Launch.Port > 65535 {
		return 0, errors.New("invalid proxy target manifest")
	}
	record, err := store.ReadRun(expectedRunID)
	if err != nil {
		return 0, err
	}
	if record.Claim != "accepted" || record.State != "ready" && (record.State != "running" || m.Launch.HealthPath != "" || m.Launch.HealthTCP) {
		return 0, errors.New("run has not passed startup gate")
	}
	if record.Child.PID <= 0 || record.Child.PGID != record.Child.PID || record.Child.SID != record.Supervisor.SID || record.Child.BootID != record.Supervisor.BootID {
		return 0, errors.New("invalid process ownership")
	}
	for _, ref := range []procid.Ref{record.Supervisor, record.Child} {
		ok, e := procid.Check(ref)
		if e != nil {
			return 0, e
		}
		if !ok {
			return 0, errors.New("recorded process is not live")
		}
	}
	members, err := runtimeMembers(record.Child, record.Supervisor)
	if err != nil {
		return 0, err
	}
	if err = checkListeners(members, m.Launch.Port); err != nil {
		return 0, err
	}
	latest, err := store.ReadRun(expectedRunID)
	if err != nil {
		return 0, err
	}
	if latest.Child != record.Child || latest.Supervisor != record.Supervisor || latest.Claim != record.Claim || latest.State != record.State {
		return 0, errors.New("run changed during target verification")
	}
	for _, ref := range []procid.Ref{record.Supervisor, record.Child} {
		ok, e := procid.Check(ref)
		if e != nil {
			return 0, e
		}
		if !ok {
			return 0, fmt.Errorf("recorded process changed during target verification")
		}
	}
	if err = checkRegistry(); err != nil {
		return 0, err
	}
	return m.Launch.Port, nil
}

func OpenProxyLog(path string) (io.WriteCloser, error) { return openLog(path) }
