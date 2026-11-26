// Reclaims ports from failed runs after verifying their processes are gone.
package runner

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func (r *Runner) reclaimableRun(runID string) (state.RunRecord, bool) {
	if runID == "" {
		return state.RunRecord{}, false
	}
	record, err := r.Store.ReadRun(runID)
	if err != nil || record.Claim != "accepted" || record.State == "stopped" {
		return record, false
	}
	currentBoot, err := procid.InCurrentBoot(record.Supervisor.BootID)
	if err != nil || !currentBoot {
		return record, false
	}
	for _, ref := range append([]procid.Ref{record.Supervisor, record.Child}, record.Members...) {
		if ref == (procid.Ref{}) {
			continue
		}
		if ref.SID != record.Supervisor.SID || ref.BootID != record.Supervisor.BootID {
			return record, false
		}
		if live, err := procid.Check(ref); err != nil || live {
			return record, false
		}
	}
	return record, verifySessionGone(record.Supervisor) == nil
}

func releaseRunPort(store *state.Store, projectID, instanceID, runID string) error {
	reg, err := store.Read()
	if err != nil {
		return err
	}
	p, err := reg.Project(projectID)
	if err != nil {
		return err
	}
	if p.Instances[instanceID].Reservations[runID] == 0 {
		return nil
	}
	return store.Update(func(reg *state.Registry) error {
		project, err := reg.Project(projectID)
		if err != nil {
			return err
		}
		instance := project.Instances[instanceID]
		if instance.RunID == runID {
			delete(instance.Reservations, runID)
			project.Instances[instanceID] = instance
		}
		return reg.SetProject(projectID, project)
	})
}

func (r *Runner) reconcileFailedRun(id, runID string) error {
	if _, ok := r.reclaimableRun(runID); !ok {
		return nil
	}
	gate, err := r.Store.LockSpawnGates(runID)
	if err != nil {
		return err
	}
	defer gate.Close()
	record, ok := r.reclaimableRun(runID)
	if !ok {
		return nil
	}
	var logErr error
	if record.State != "failed" || record.LastExit == nil {
		record.LastExit = &state.Exit{Reason: state.SupervisorLostReason, Time: time.Now().UTC()}
		record.State = "failed"
		lock, err := r.Store.LockRunWriter(runID)
		if err != nil {
			return err
		}
		defer lock.Close()
		if err := r.Store.WriteRun(runID, record); err != nil {
			return err
		}
		log, err := openLog(filepath.Join(r.Store.Root, "runs", runID, "application.log"))
		if err == nil {
			_, err = fmt.Fprintf(log, "[rundevrun] %s %s\n", record.LastExit.Time.Format(time.RFC3339), record.LastExit)
			err = errors.Join(err, log.Close())
		}
		logErr = err
	}
	return errors.Join(logErr, releaseRunPort(r.Store, strings.SplitN(id, "/", 2)[0], id, runID))
}

type reservedPort struct {
	port       int
	instanceID string
	runID      string
}

func (p *reservedPort) Error() string {
	return fmt.Sprintf("port %d is reserved by instance %s (run %s)", p.port, p.instanceID, p.runID)
}

func (p *reservedPort) Actions() []string {
	parts := strings.Split(p.instanceID, "/")
	return []string{fmt.Sprintf("rundevrun down %s --project %s --worktree %s", parts[2], parts[0], parts[1])}
}
