package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOperationJournalAndReferences(t *testing.T) {
	s, e := New(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	op := Operation{ID: "op", Kind: "switch", Status: "running", Plan: json.RawMessage(`{"version":1}`), Runs: map[string]string{"instance": "run"}, Steps: []OperationStep{{ID: "stop:service", Status: "started"}}}
	if e = s.WriteOperation(op); e != nil {
		t.Fatal(e)
	}
	loaded, e := s.ReadOperation(op.ID)
	if e != nil || loaded.Steps[0].Status != "started" {
		t.Fatalf("read: %+v %v", loaded, e)
	}
	refs, e := s.OperationRunReferences()
	if e != nil || !refs["run"] {
		t.Fatalf("references: %v %v", refs, e)
	}
	path, e := s.OperationPath(op.ID)
	if e != nil {
		t.Fatal(e)
	}
	if info, e := os.Stat(path); e != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode: %v %v", info, e)
	}
	op.Status = "completed"
	op.Steps[0].Status = "completed"
	if e = s.WriteOperation(op); e != nil {
		t.Fatal(e)
	}
	refs, e = s.OperationRunReferences()
	if e != nil || refs["run"] {
		t.Fatalf("completed references: %v %v", refs, e)
	}
}
