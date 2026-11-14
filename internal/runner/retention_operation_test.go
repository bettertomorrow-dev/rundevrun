package runner

import (
	"encoding/json"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/state"
)

func TestRunReferencesProtectsUnfinishedOperation(t *testing.T) {
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	op := state.Operation{ID: "pending-switch", Kind: "switch", Status: "started", Plan: json.RawMessage(`{}`), Runs: map[string]string{"instance": "prepared-run"}}
	if err = store.WriteOperation(op); err != nil {
		t.Fatal(err)
	}
	r := New(store, "")
	reg, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	refs, err := r.runReferences(reg)
	if err != nil || !refs["prepared-run"] {
		t.Fatalf("unfinished operation lost its run: %v, %v", refs, err)
	}
	op.Status = "completed"
	if err = store.WriteOperation(op); err != nil {
		t.Fatal(err)
	}
	refs, err = r.runReferences(reg)
	if err != nil || refs["prepared-run"] {
		t.Fatalf("completed operation still protects unreferenced run: %v, %v", refs, err)
	}
}
