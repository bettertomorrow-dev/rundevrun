// Tests state launch behavior.
package state

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLaunchHistoryKeepsPerServiceOutcomesAndNewestAttempt(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "state")}
	now := time.Now().UTC()
	old := LaunchResult{Attempt: "old", ProjectID: "p", InstanceID: "p/w/a", StartedAt: now, Error: "build failed"}
	other := LaunchResult{Attempt: "new", ProjectID: "p", InstanceID: "p/w/b", StartedAt: now.Add(time.Second)}
	if err := s.RecordLaunch([]LaunchResult{old}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordLaunch([]LaunchResult{other}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordLaunch([]LaunchResult{old}); err != nil {
		t.Fatal(err)
	}
	h, err := s.ReadLaunchHistory()
	if err != nil {
		t.Fatal(err)
	}
	if h.Services[old.InstanceID].Error == "" || h.Projects["p"].Attempt != "new" {
		t.Fatalf("lost failure or newest attempt: %+v", h)
	}
	fixed := old
	fixed.Attempt = "fixed"
	fixed.StartedAt = now.Add(2 * time.Second)
	fixed.Error = ""
	if err := s.RecordLaunch([]LaunchResult{fixed}); err != nil {
		t.Fatal(err)
	}
	h, err = s.ReadLaunchHistory()
	if err != nil {
		t.Fatal(err)
	}
	if h.Services[old.InstanceID].Error != "" || h.Projects["p"].Attempt != "fixed" {
		t.Fatalf("successful retry did not clear failure: %+v", h)
	}
}
