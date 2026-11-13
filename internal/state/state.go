// Package state persists runner records. Project values are decoded separately
// so one damaged project does not prevent access to the others.
package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
	"golang.org/x/sys/unix"
)

var ErrBusy = errors.New("state busy")
var ErrCorrupt = errors.New("state corrupt")

type Store struct{ Root string }
type Registry struct {
	Schema   int                        `json:"schema"`
	Revision uint64                     `json:"revision"`
	Projects map[string]json.RawMessage `json:"projects"`
}
type Worktree struct {
	ID       string `json:"id"`
	Root     string `json:"root"`
	Branch   string `json:"branch,omitempty"`
	Detached bool   `json:"detached"`
}
type Project struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Root         string              `json:"root"`
	GitCommonDir string              `json:"git_common_dir,omitempty"`
	Worktrees    []Worktree          `json:"worktrees,omitempty"`
	Instances    map[string]Instance `json:"instances,omitempty"`
}
type Instance struct {
	ID                string            `json:"id"`
	Service           string            `json:"service"`
	WorktreeID        string            `json:"worktree_id"`
	State             string            `json:"state"`
	RunID             string            `json:"run_id,omitempty"`
	DesiredBindings   map[string]string `json:"desired_bindings,omitempty"`
	EffectiveBindings map[string]string `json:"effective_bindings,omitempty"`
	Reservations      map[string]int    `json:"reservations,omitempty"`
	Intent            *Intent           `json:"intent,omitempty"`
}
type Intent struct {
	OperationID string `json:"operation_id"`
	RunID       string `json:"run_id"`
	Phase       string `json:"phase"`
}
type StopContract struct {
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd"`
	Command string            `json:"command,omitempty"`
	Hooks   map[string]string `json:"hooks,omitempty"`
	RunDir  string            `json:"run_dir"`
}
type Manifest struct {
	RunID             string            `json:"run_id"`
	InstanceID        string            `json:"instance_id"`
	OperationID       string            `json:"operation_id"`
	Digest            string            `json:"digest"`
	Addresses         map[string]string `json:"addresses,omitempty"`
	EffectiveBindings map[string]string `json:"effective_bindings,omitempty"`
	StopContract      StopContract      `json:"stop_contract"`
}
type RunRecord struct {
	Claim      string       `json:"claim,omitempty"`
	Supervisor procid.Ref   `json:"supervisor"`
	Child      procid.Ref   `json:"child"`
	Members    []procid.Ref `json:"members,omitempty"`
	State      string       `json:"state"`
}

func empty() Registry { return Registry{Schema: 1, Projects: map[string]json.RawMessage{}} }
func New(root string) (*Store, error) {
	s := &Store{Root: root}
	if err := s.ensure(); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Store) ensure() error {
	if s.Root == "" {
		return errors.New("empty state root")
	}
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return err
	}
	return privateDir(s.Root)
}
func privateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe directory %s", path)
	}
	return os.Chmod(path, 0700)
}
func safeName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\")
}
func readFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("unsafe file %s", path)
	}
	return io.ReadAll(f)
}
func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON")
		}
		return err
	}
	return nil
}
func (s *Store) Read() (Registry, error) {
	r := empty()
	info, err := os.Lstat(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return Registry{}, fmt.Errorf("unsafe state root %s", s.Root)
	}
	path := filepath.Join(s.Root, "registry.json")
	b, err := readFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return Registry{}, err
	}
	var disk struct {
		Schema   *int                       `json:"schema"`
		Revision *uint64                    `json:"revision"`
		Projects map[string]json.RawMessage `json:"projects"`
	}
	if err = decode(b, &disk); err != nil || disk.Schema == nil || *disk.Schema != 1 || disk.Revision == nil || disk.Projects == nil {
		return Registry{}, fmt.Errorf("%w: %s: invalid registry: %v", ErrCorrupt, path, err)
	}
	return Registry{Schema: *disk.Schema, Revision: *disk.Revision, Projects: disk.Projects}, nil
}
func (r Registry) Project(id string) (Project, error) {
	var p Project
	b, ok := r.Projects[id]
	if !ok {
		return p, os.ErrNotExist
	}
	if err := decode(b, &p); err != nil || p.ID != id || p.Name == "" || p.Root == "" {
		return Project{}, fmt.Errorf("%w: project %s: %v", ErrCorrupt, id, err)
	}
	worktrees := map[string]bool{}
	for _, w := range p.Worktrees {
		if w.ID == "" || w.Root == "" || worktrees[w.ID] {
			return Project{}, fmt.Errorf("%w: project %s: invalid worktree", ErrCorrupt, id)
		}
		worktrees[w.ID] = true
	}
	for key, i := range p.Instances {
		if i.Service == "" || !worktrees[i.WorktreeID] || i.ID != key || key != id+"/"+i.WorktreeID+"/"+i.Service || !oneOf(i.State, "stopped", "starting", "running", "ready", "unhealthy", "failed") {
			return Project{}, fmt.Errorf("%w: project %s: invalid instance %s", ErrCorrupt, id, key)
		}
		if i.RunID != "" && !safeName(i.RunID) {
			return Project{}, fmt.Errorf("%w: invalid run id", ErrCorrupt)
		}
		if i.Intent != nil && (i.Intent.OperationID == "" || i.Intent.RunID == "" || i.Intent.RunID != i.RunID || !oneOf(i.Intent.Phase, "preparing", "prepared", "spawning", "started", "stopping", "stopped", "hook_failed", "complete", "interrupted")) {
			return Project{}, fmt.Errorf("%w: project %s: invalid intent for %s", ErrCorrupt, id, key)
		}
	}
	return p, nil
}
func oneOf(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}
func (r *Registry) SetProject(id string, p Project) error {
	if id == "" || p.ID != id || p.Name == "" || p.Root == "" {
		return errors.New("invalid project")
	}
	b, err := json.Marshal(p)
	if err == nil {
		if r.Projects == nil {
			r.Projects = map[string]json.RawMessage{}
		}
		r.Projects[id] = b
	}
	return err
}
func (s *Store) Update(fn func(*Registry) error) error {
	if err := s.ensure(); err != nil {
		return err
	}
	locks, err := s.lock([]string{"registry"}, false)
	if err != nil {
		return err
	}
	defer locks.Close()
	r, err := s.Read()
	if err != nil {
		return err
	}
	if err = fn(&r); err != nil {
		return err
	}
	r.Schema = 1
	r.Revision++
	return s.write(filepath.Join(s.Root, "registry.json"), r)
}
func (s *Store) write(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if st, err := os.Lstat(path); err == nil && (!st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("unsafe file %s", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

type Locks struct{ files []*os.File }

func (l *Locks) Close() error {
	var err error
	for i := len(l.files) - 1; i >= 0; i-- {
		unix.Flock(int(l.files[i].Fd()), unix.LOCK_UN)
		if e := l.files[i].Close(); err == nil {
			err = e
		}
	}
	return err
}
func (s *Store) lock(names []string, nonblock bool) (*Locks, error) {
	if err := s.ensure(); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.Root, "locks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	names = append([]string(nil), names...)
	sort.Strings(names)
	l := &Locks{}
	for i, n := range names {
		if !safeName(n) {
			l.Close()
			return nil, errors.New("invalid lock name")
		}
		if i > 0 && n == names[i-1] {
			continue
		}
		f, err := os.OpenFile(filepath.Join(dir, n), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
		if err != nil {
			l.Close()
			return nil, err
		}
		if err = f.Chmod(0600); err != nil {
			f.Close()
			l.Close()
			return nil, err
		}
		flags := unix.LOCK_EX
		if nonblock {
			flags |= unix.LOCK_NB
		}
		if err = unix.Flock(int(f.Fd()), flags); err != nil {
			f.Close()
			l.Close()
			if errors.Is(err, unix.EWOULDBLOCK) {
				return nil, ErrBusy
			}
			return nil, err
		}
		l.files = append(l.files, f)
	}
	return l, nil
}

func lockName(prefix, id string) string {
	return fmt.Sprintf("%s-%x", prefix, sha256.Sum256([]byte(id)))
}

// LockInstances acquires all instance operation locks without waiting. Callers
// acquire spawn gates afterwards, then update the registry.
func (s *Store) LockInstances(ids ...string) (*Locks, error) {
	names := make([]string, len(ids))
	for i, id := range ids {
		if id == "" {
			return nil, errors.New("empty instance id")
		}
		names[i] = lockName("instance", id)
	}
	return s.lock(names, true)
}
func (s *Store) LockSpawnGates(ids ...string) (*Locks, error) {
	names := make([]string, len(ids))
	for i, id := range ids {
		if !safeName(id) {
			return nil, errors.New("invalid run id")
		}
		names[i] = lockName("spawn", id)
	}
	return s.lock(names, true)
}
func (s *Store) LockRunWriter(id string) (*Locks, error) {
	if !safeName(id) {
		return nil, errors.New("invalid run id")
	}
	return s.lock([]string{lockName("writer", id)}, true)
}
func (s *Store) runDir(id string, create bool) (string, error) {
	if !safeName(id) {
		return "", errors.New("invalid run id")
	}
	runs := filepath.Join(s.Root, "runs")
	dir := filepath.Join(runs, id)
	if create {
		if err := s.ensure(); err != nil {
			return "", err
		}
		if err := os.MkdirAll(runs, 0700); err != nil {
			return "", err
		}
		if err := privateDir(runs); err != nil {
			return "", err
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", err
		}
		if err := privateDir(dir); err != nil {
			return "", err
		}
	} else {
		for _, p := range []string{s.Root, runs, dir} {
			st, err := os.Lstat(p)
			if err != nil {
				return "", err
			}
			if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("unsafe directory %s", p)
			}
		}
	}
	return dir, nil
}
func (s *Store) WriteManifest(id string, m Manifest) error {
	if m.RunID != id {
		return errors.New("manifest run id mismatch")
	}
	dir, err := s.runDir(id, true)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "manifest.json")
	if _, err = os.Lstat(path); err == nil {
		return errors.New("manifest already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.write(path, m)
}
func (s *Store) ReadManifest(id string) (Manifest, error) {
	var m Manifest
	err := s.readRun(id, "manifest.json", &m)
	if err == nil && m.RunID != id {
		err = fmt.Errorf("%w: manifest run id mismatch", ErrCorrupt)
	}
	return m, err
}

// WriteRun is reserved for the supervisor and should be protected by LockRunWriter.
func (s *Store) WriteRun(id string, r RunRecord) error {
	dir, err := s.runDir(id, true)
	if err != nil {
		return err
	}
	return s.write(filepath.Join(dir, "supervisor.json"), r)
}
func (s *Store) ReadRun(id string) (RunRecord, error) {
	var r RunRecord
	err := s.readRun(id, "supervisor.json", &r)
	return r, err
}
func (s *Store) readRun(id, name string, v any) error {
	dir, err := s.runDir(id, false)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	b, err := readFile(path)
	if err != nil {
		return err
	}
	if err = decode(b, v); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrCorrupt, path, err)
	}
	return nil
}

// Inspect reports liveness without changing an intent or reservation.
func (s *Store) Inspect(i Instance) (string, error) {
	if i.RunID == "" {
		return i.State, nil
	}
	m, err := s.ReadManifest(i.RunID)
	if err != nil {
		return "", err
	}
	if m.InstanceID != i.ID {
		return "", fmt.Errorf("%w: manifest instance mismatch", ErrCorrupt)
	}
	r, err := s.ReadRun(i.RunID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && i.Intent != nil {
			return "starting", nil
		}
		return "", err
	}
	live, err := runLive(r)
	if err != nil {
		return "", err
	}
	if live {
		return r.State, nil
	}
	if r.State == "stopped" || r.State == "failed" {
		return r.State, nil
	}
	return "stale", nil
}

// Recover marks a verified dead run failed under its operation lock and gate.
// A claimed run with incomplete identity is uncertain and is not changed.
func (s *Store) Recover(projectID, instanceID string) error {
	locks, err := s.LockInstances(instanceID)
	if err != nil {
		return err
	}
	defer locks.Close()
	reg, err := s.Read()
	if err != nil {
		return err
	}
	p, err := reg.Project(projectID)
	if err != nil {
		return err
	}
	i, ok := p.Instances[instanceID]
	if !ok {
		return os.ErrNotExist
	}
	if i.RunID == "" {
		return nil
	}
	iRunID := i.RunID
	gate, err := s.LockSpawnGates(i.RunID)
	if err != nil {
		return err
	}
	defer gate.Close()
	return s.Update(func(reg *Registry) error {
		p, err := reg.Project(projectID)
		if err != nil {
			return err
		}
		i, ok := p.Instances[instanceID]
		if !ok {
			return os.ErrNotExist
		}
		if i.RunID == "" {
			return nil
		}
		if i.RunID != iRunID {
			return errors.New("run changed during recovery")
		}
		m, err := s.ReadManifest(i.RunID)
		if err != nil {
			return err
		}
		if m.InstanceID != i.ID {
			return fmt.Errorf("%w: manifest instance mismatch", ErrCorrupt)
		}
		r, err := s.ReadRun(i.RunID)
		if errors.Is(err, os.ErrNotExist) {
			if i.Intent == nil {
				return err
			}
			i.State = "failed"
			i.Intent.Phase = "interrupted"
		} else if err != nil {
			return err
		} else {
			live, err := runLive(r)
			if err != nil {
				return err
			}
			if live {
				return nil
			}
			if r.State == "stopped" || r.State == "failed" {
				i.State = r.State
				p.Instances[instanceID] = i
				return reg.SetProject(projectID, p)
			}
			i.State = "failed"
			if i.Intent != nil {
				i.Intent.Phase = "interrupted"
			}
		}
		p.Instances[instanceID] = i
		return reg.SetProject(projectID, p)
	})
}

func runLive(r RunRecord) (bool, error) {
	if r.Claim == "accepted" && r.Supervisor.PID == 0 {
		return false, errors.New("uncertain claimed run ownership")
	}
	refs := append([]procid.Ref{r.Supervisor, r.Child}, r.Members...)
	anyLive := false
	for _, ref := range refs {
		if ref == (procid.Ref{}) {
			continue
		}
		live, err := procid.Check(ref)
		if err != nil {
			return false, fmt.Errorf("uncertain process ownership: %w", err)
		}
		if live {
			anyLive = true
		}
	}
	return anyLive, nil
}
