package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

const retainedRuns = 5

type retainedCandidate struct {
	id    string
	stamp time.Time
	dir   os.FileInfo
}

// pruneRuns is called with the instance operation lock held. Unverifiable
// records are retained; they are never counted against the historical limit.
func (r *Runner) pruneRuns(instanceID string) error {
	reg, err := r.Store.Read()
	if err != nil {
		return err
	}
	refs, err := runReferences(reg)
	if err != nil {
		return err
	}
	rootInfo, err := os.Lstat(r.Store.Root)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm() != 0700 {
		return errors.New("unsafe runtime root")
	}
	root, err := os.OpenRoot(r.Store.Root)
	if err != nil {
		return err
	}
	defer root.Close()
	openedInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(rootInfo, openedInfo) {
		return errors.New("runtime root changed")
	}
	runsInfo, err := root.Lstat("runs")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !runsInfo.IsDir() || runsInfo.Mode()&os.ModeSymlink != 0 || runsInfo.Mode().Perm() != 0700 {
		return errors.New("unsafe runs directory")
	}
	runs, err := root.OpenRoot("runs")
	if err != nil {
		return err
	}
	defer runs.Close()
	openedInfo, err = runs.Stat(".")
	if err != nil || !os.SameFile(runsInfo, openedInfo) {
		return errors.New("runs directory changed")
	}
	f, err := runs.Open(".")
	if err != nil {
		return err
	}
	entries, err := f.ReadDir(-1)
	closeErr := f.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return err
	}
	candidates := make([]retainedCandidate, 0)
	for _, entry := range entries {
		id := entry.Name()
		if refs[id] {
			continue
		}
		info, err := runs.Lstat(id)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
			continue
		}
		manifestInfo, err := runs.Lstat(filepath.Join(id, "manifest.json"))
		if err != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Mode().Perm() != 0600 {
			continue
		}
		manifest, err := r.Store.ReadManifest(id)
		if err != nil || manifest.RunID != id || manifest.InstanceID != instanceID || manifest.OperationID == "" {
			continue
		}
		if !r.completedRun(id) {
			continue
		}
		candidates = append(candidates, retainedCandidate{id: id, stamp: manifestInfo.ModTime(), dir: info})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].stamp.Equal(candidates[j].stamp) {
			return candidates[i].id > candidates[j].id
		}
		return candidates[i].stamp.After(candidates[j].stamp)
	})
	if len(candidates) <= retainedRuns {
		return nil
	}
	for _, candidate := range candidates[retainedRuns:] {
		gate, err := r.Store.LockSpawnGates(candidate.id)
		if errors.Is(err, state.ErrBusy) {
			continue
		}
		if err != nil {
			return err
		}
		err = r.removeCompleted(runs, instanceID, candidate)
		closeErr := gate.Close()
		if err = errors.Join(err, closeErr); err != nil {
			return err
		}
	}
	return nil
}

func runReferences(reg state.Registry) (map[string]bool, error) {
	refs := map[string]bool{}
	for id := range reg.Projects {
		p, err := reg.Project(id)
		if err != nil {
			return nil, err
		}
		for _, i := range p.Instances {
			if i.RunID != "" {
				refs[i.RunID] = true
			}
			if i.Intent != nil {
				refs[i.Intent.RunID] = true
			}
			if i.Preparation != nil {
				refs[i.Preparation.RunID] = true
			}
			if i.Hook != nil {
				refs[i.Hook.RunID] = true
			}
			for runID := range i.Reservations {
				refs[runID] = true
			}
		}
	}
	return refs, nil
}
func (r *Runner) removeCompleted(runs *os.Root, instanceID string, candidate retainedCandidate) error {
	reg, err := r.Store.Read()
	if err != nil {
		return err
	}
	refs, err := runReferences(reg)
	if err != nil {
		return err
	}
	if refs[candidate.id] {
		return nil
	}
	info, err := runs.Lstat(candidate.id)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 || !os.SameFile(info, candidate.dir) {
		return nil
	}
	manifestInfo, err := runs.Lstat(filepath.Join(candidate.id, "manifest.json"))
	if err != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Mode().Perm() != 0600 || !manifestInfo.ModTime().Equal(candidate.stamp) {
		return nil
	}
	manifest, err := r.Store.ReadManifest(candidate.id)
	if err != nil || manifest.RunID != candidate.id || manifest.InstanceID != instanceID || manifest.OperationID == "" {
		return nil
	}
	if !r.completedRun(candidate.id) {
		return nil
	}
	if err = runs.RemoveAll(candidate.id); err != nil {
		return fmt.Errorf("remove completed run %s: %w", candidate.id, err)
	}
	return nil
}

func (r *Runner) completedRun(id string) bool {
	rec, runErr := r.Store.ReadRun(id)
	prep, prepErr := r.Store.ReadPreparation(id)
	if runErr != nil && !errors.Is(runErr, os.ErrNotExist) {
		return false
	}
	if prepErr != nil && !errors.Is(prepErr, os.ErrNotExist) {
		return false
	}
	if errors.Is(runErr, os.ErrNotExist) && errors.Is(prepErr, os.ErrNotExist) {
		return false
	}
	if runErr == nil {
		if rec.State != "stopped" && rec.State != "failed" {
			return false
		}
		if rec.Supervisor == (procid.Ref{}) {
			return false
		}
		refs := append([]procid.Ref{rec.Supervisor, rec.Child}, rec.Members...)
		for _, ref := range refs {
			if ref == (procid.Ref{}) {
				continue
			}
			live, err := procid.Check(ref)
			if err != nil || live {
				return false
			}
		}
		checks := append([]procid.Ref{rec.Child}, rec.Members...)
		if rec.Child == (procid.Ref{}) {
			checks = append(checks, rec.Supervisor)
		} else if rec.Child.SID != rec.Supervisor.SID || rec.Child.BootID != rec.Supervisor.BootID {
			return false
		}
		seen := map[int]bool{}
		for _, ref := range checks {
			if ref == (procid.Ref{}) || seen[ref.PGID] {
				continue
			}
			seen[ref.PGID] = true
			if err := verifySessionGone(ref); err != nil {
				return false
			}
		}
	}
	if prepErr == nil {
		if prep.Child.PID <= 0 || prep.Child.PGID != prep.Child.PID || prep.Child.SID != prep.Child.PID || prep.Child.Identity == "" || prep.Child.BootID == "" {
			return false
		}
		refs := append([]procid.Ref{prep.Child}, prep.Members...)
		for _, ref := range refs {
			if ref.PGID != prep.Child.PGID || ref.SID != prep.Child.SID || ref.BootID != prep.Child.BootID {
				return false
			}
			live, err := procid.Check(ref)
			if err != nil || live {
				return false
			}
		}
		if err := verifySessionGone(prep.Child); err != nil {
			return false
		}
	}
	return true
}
