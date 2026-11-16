// Persists lifecycle operation journals and locks.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
)

type Operation struct {
	ID     string            `json:"id"`
	Kind   string            `json:"kind"`
	Status string            `json:"status"`
	Owner  procid.Ref        `json:"owner"`
	Plan   json.RawMessage   `json:"plan"`
	Runs   map[string]string `json:"runs,omitempty"`
	Steps  []OperationStep   `json:"steps"`
}
type OperationStep struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func (s *Store) operationDir(create bool) (string, error) {
	if create {
		if e := s.ensure(); e != nil {
			return "", e
		}
		dir := filepath.Join(s.Root, "operations")
		if e := os.MkdirAll(dir, 0700); e != nil {
			return "", e
		}
		if e := privateDir(dir); e != nil {
			return "", e
		}
		return dir, nil
	}
	rootInfo, e := os.Lstat(s.Root)
	if e != nil {
		return "", e
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm() != 0700 {
		return "", fmt.Errorf("unsafe state root")
	}
	dir := filepath.Join(s.Root, "operations")
	st, e := os.Lstat(dir)
	if e != nil {
		return "", e
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0700 {
		return "", fmt.Errorf("unsafe operations directory")
	}
	return dir, nil
}
func (s *Store) OperationPath(id string) (string, error) {
	if !safeName(id) {
		return "", errors.New("invalid operation id")
	}
	return filepath.Join(s.Root, "operations", id+".json"), nil
}
func validOperation(op Operation) bool {
	if !safeName(op.ID) || op.Kind != "switch" || !oneOf(op.Status, "started", "running", "completed", "failed", "interrupted") || !json.Valid(op.Plan) {
		return false
	}
	seen := map[string]bool{}
	for _, step := range op.Steps {
		if step.ID == "" || seen[step.ID] || !oneOf(step.Status, "not_run", "started", "completed", "failed", "interrupted") {
			return false
		}
		seen[step.ID] = true
	}
	for _, id := range op.Runs {
		if !safeName(id) {
			return false
		}
	}
	return true
}
func (s *Store) WriteOperation(op Operation) error {
	if !validOperation(op) {
		return errors.New("invalid operation")
	}
	dir, e := s.operationDir(true)
	if e != nil {
		return e
	}
	return s.write(filepath.Join(dir, op.ID+".json"), op)
}
func (s *Store) ReadOperation(id string) (Operation, error) {
	var op Operation
	if !safeName(id) {
		return op, errors.New("invalid operation id")
	}
	dir, e := s.operationDir(false)
	if e != nil {
		return op, e
	}
	b, e := readFile(filepath.Join(dir, id+".json"))
	if e != nil {
		return op, e
	}
	if e = decode(b, &op); e != nil || op.ID != id || !validOperation(op) {
		return Operation{}, fmt.Errorf("%w: invalid operation %s", ErrCorrupt, id)
	}
	return op, nil
}

func (s *Store) ListOperations() ([]Operation, error) {
	dir, e := s.operationDir(false)
	if errors.Is(e, os.ErrNotExist) {
		return []Operation{}, nil
	}
	if e != nil {
		return nil, e
	}
	entries, e := os.ReadDir(dir)
	if e != nil {
		return nil, e
	}
	out := []Operation{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		op, e := s.ReadOperation(name[:len(name)-5])
		if e != nil {
			return nil, e
		}
		out = append(out, op)
	}
	return out, nil
}

func (s *Store) OperationRunReferences() (map[string]bool, error) {
	refs := map[string]bool{}
	operations, e := s.ListOperations()
	if e != nil {
		return nil, e
	}
	for _, op := range operations {
		if op.Status == "completed" {
			continue
		}
		for _, id := range op.Runs {
			if !safeName(id) {
				return nil, fmt.Errorf("%w: invalid journal run", ErrCorrupt)
			}
			refs[id] = true
		}
	}
	return refs, nil
}
