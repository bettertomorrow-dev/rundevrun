// LaunchResult records the latest completed up attempt for one service.
package state

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

type LaunchResult struct {
	Attempt    string    `json:"attempt"`
	ProjectID  string    `json:"project_id"`
	InstanceID string    `json:"instance_id"`
	StartedAt  time.Time `json:"started_at"`
	Error      string    `json:"error,omitempty"`
}

type LaunchHistory struct {
	Services map[string]LaunchResult `json:"services"`
	Projects map[string]LaunchResult `json:"projects"`
}

func (s *Store) ReadLaunchHistory() (LaunchHistory, error) {
	h := LaunchHistory{Services: map[string]LaunchResult{}, Projects: map[string]LaunchResult{}}

	if _, err := s.Read(); err != nil {
		return h, err
	}
	b, err := readFile(filepath.Join(s.Root, "launches.json"))
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	if err := decode(b, &h); err != nil || h.Services == nil || h.Projects == nil {
		return LaunchHistory{}, ErrCorrupt
	}
	return h, nil
}

func (s *Store) RecordLaunch(results []LaunchResult) error {
	if len(results) == 0 {
		return nil
	}
	locks, err := s.lock([]string{"launches"}, false)
	if err != nil {
		return err
	}
	defer locks.Close()
	h, err := s.ReadLaunchHistory()
	if err != nil {
		return err
	}
	for _, r := range results {
		if r.Attempt == "" || r.ProjectID == "" || r.InstanceID == "" || r.StartedAt.IsZero() {
			return errors.New("invalid launch result")
		}
		if old, ok := h.Services[r.InstanceID]; !ok || !r.StartedAt.Before(old.StartedAt) {
			h.Services[r.InstanceID] = r
		}
		if old, ok := h.Projects[r.ProjectID]; !ok || r.StartedAt.After(old.StartedAt) || (r.Attempt == old.Attempt && old.Error == "" && r.Error != "") {
			h.Projects[r.ProjectID] = r
		}
	}
	return s.write(filepath.Join(s.Root, "launches.json"), h)
}
