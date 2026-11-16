// Implements tui model.
package tui

import (
	"sort"

	"github.com/bettertomorrow-dev/rundevrun/internal/monitor"
)

type Model struct {
	Snapshot    monitor.Snapshot
	Tab         int
	AllProjects bool
	Selected    string
	Status      string
	Logs        string
	ShowLogs    bool
	Initialized bool
}

func (m *Model) Apply(s monitor.Snapshot) {
	oldTab := m.Tab
	m.Snapshot = s
	if !m.Initialized {
		if s.PreferErrors {
			m.Tab = 1
		}
		m.Initialized = true
	} else {
		m.Tab = oldTab
	}
	rows := m.rows()
	found := false
	for _, row := range rows {
		if row.ID == m.Selected {
			found = true
			break
		}
	}
	if !found {
		m.Selected = ""
		if len(rows) > 0 {
			m.Selected = rows[0].ID
		}
	}
}

func (m Model) rows() []monitor.Service {
	out := make([]monitor.Service, 0)
	for _, s := range m.Snapshot.Services {
		if !m.AllProjects && m.Snapshot.CurrentProjectID != "" && s.ProjectID != m.Snapshot.CurrentProjectID {
			continue
		}
		if m.Tab == 0 && s.State != "running" && s.State != "ready" && s.State != "starting" && s.State != "unhealthy" && s.State != "unknown" {
			continue
		}
		if m.Tab == 1 && s.Error == "" {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectName != out[j].ProjectName {
			return out[i].ProjectName < out[j].ProjectName
		}
		if out[i].ProjectID != out[j].ProjectID {
			return out[i].ProjectID < out[j].ProjectID
		}
		if out[i].Worktree != out[j].Worktree {
			return out[i].Worktree < out[j].Worktree
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (m *Model) Move(delta int) {
	rows := m.rows()
	if len(rows) == 0 {
		m.Selected = ""
		return
	}
	idx := 0
	for i, r := range rows {
		if r.ID == m.Selected {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(rows)) % len(rows)
	m.Selected = rows[idx].ID
}

func (m *Model) ToggleTab()   { m.Tab = 1 - m.Tab; m.SelectFirst() }
func (m *Model) ToggleScope() { m.AllProjects = !m.AllProjects; m.SelectFirst() }
func (m *Model) SelectFirst() {
	rows := m.rows()
	m.Selected = ""
	if len(rows) > 0 {
		m.Selected = rows[0].ID
	}
}
func (m Model) SelectedService() (monitor.Service, bool) {
	for _, r := range m.rows() {
		if r.ID == m.Selected {
			return r, true
		}
	}
	return monitor.Service{}, false
}
