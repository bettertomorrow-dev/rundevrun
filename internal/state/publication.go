package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
)

// PublicationRegistry is written by the CLI under the publication registry lock.
// The daemon writes only its own per-publication runtime record.
type PublicationRegistry struct {
	Version      int                    `json:"version"`
	Revision     uint64                 `json:"revision"`
	Publications map[string]Publication `json:"publications"`
}
type Publication struct {
	ID          string            `json:"id"`
	ProjectID   string            `json:"project_id"`
	RootID      string            `json:"root_id"`
	RootRunID   string            `json:"root_run_id"`
	RoutedRuns  map[string]string `json:"routed_runs,omitempty"`
	Mode        string            `json:"mode"`
	Phase       string            `json:"phase"`
	PreviousID  string            `json:"previous_id,omitempty"`
	Cloudflared string            `json:"cloudflared"`
	Owner       procid.Ref        `json:"owner"`
	Salt        []byte            `json:"salt"`
	Verifier    []byte            `json:"verifier"`
	SigningKey  []byte            `json:"signing_key"`
}

// PublicationRuntime is exclusively written by the publication supervisor.
type PublicationRuntime struct {
	ID         string       `json:"id"`
	State      string       `json:"state"`
	URL        string       `json:"url,omitempty"`
	Supervisor procid.Ref   `json:"supervisor"`
	Tunnel     procid.Ref   `json:"tunnel"`
	Members    []procid.Ref `json:"members,omitempty"`
}

func (s *Store) LockPublicationRegistry() (*Locks, error) {
	return s.lock([]string{"publications"}, true)
}
func (s *Store) LockPublicationGate(id string) (*Locks, error) {
	if !safeName(id) {
		return nil, errors.New("invalid publication id")
	}
	return s.lock([]string{lockName("publication", id)}, true)
}
func (s *Store) publicationDir(id string, create bool) (string, error) {
	if !safeName(id) {
		return "", errors.New("invalid publication id")
	}
	root := filepath.Join(s.Root, "publications")
	dir := filepath.Join(root, id)
	if create {
		if e := s.ensure(); e != nil {
			return "", e
		}
		for _, path := range []string{root, dir} {
			if e := os.MkdirAll(path, 0700); e != nil {
				return "", e
			}
			if e := privateDir(path); e != nil {
				return "", e
			}
		}
		return dir, nil
	}
	for _, path := range []string{s.Root, root, dir} {
		st, e := os.Lstat(path)
		if e != nil {
			return "", e
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0700 {
			return "", fmt.Errorf("unsafe publication directory %s", path)
		}
	}
	return dir, nil
}
func (s *Store) ReadPublications() (PublicationRegistry, error) {
	result := PublicationRegistry{Version: 1, Publications: map[string]Publication{}}
	rootInfo, e := os.Lstat(s.Root)
	if errors.Is(e, os.ErrNotExist) {
		return result, nil
	}
	if e != nil {
		return PublicationRegistry{}, e
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || rootInfo.Mode().Perm() != 0700 {
		return PublicationRegistry{}, errors.New("unsafe state root")
	}
	b, e := readFile(filepath.Join(s.Root, "publications.json"))
	if errors.Is(e, os.ErrNotExist) {
		return result, nil
	}
	if e != nil {
		return PublicationRegistry{}, e
	}
	var disk struct {
		Version      *int                   `json:"version"`
		Revision     *uint64                `json:"revision"`
		Publications map[string]Publication `json:"publications"`
	}
	if e = decode(b, &disk); e != nil || disk.Version == nil || *disk.Version != 1 || disk.Revision == nil || disk.Publications == nil {
		return PublicationRegistry{}, fmt.Errorf("%w: invalid publication registry", ErrCorrupt)
	}
	result = PublicationRegistry{Version: *disk.Version, Revision: *disk.Revision, Publications: disk.Publications}
	if e = validatePublications(result); e != nil {
		return PublicationRegistry{}, e
	}

	return result, nil
}

func validatePublications(reg PublicationRegistry) error {
	if reg.Version != 1 || reg.Publications == nil {
		return fmt.Errorf("%w: invalid publication registry", ErrCorrupt)
	}
	active := map[string]bool{}
	for id, p := range reg.Publications {
		parts := strings.Split(p.RootID, "/")
		if !safeName(id) || p.ID != id || len(parts) != 3 || parts[0] != p.ProjectID || parts[1] == "" || parts[2] == "" || !safeName(p.RootRunID) || p.Mode != "live" || !oneOf(p.Phase, "preparing", "active", "closing", "closed", "failed") || len(p.Salt) < 16 || len(p.Verifier) != 32 || len(p.SigningKey) < 32 || !filepath.IsAbs(p.Cloudflared) || p.Owner.PID <= 0 || p.Owner.PGID <= 0 || p.Owner.SID <= 0 || p.Owner.Identity == "" || p.Owner.BootID == "" || (p.PreviousID != "" && !safeName(p.PreviousID)) {
			return fmt.Errorf("%w: invalid publication %s", ErrCorrupt, id)
		}
		if p.Phase == "active" {
			if active[p.RootID] {
				return fmt.Errorf("%w: duplicate active publication", ErrCorrupt)
			}
			active[p.RootID] = true
		}
		for target, run := range p.RoutedRuns {
			targetParts := strings.Split(target, "/")
			if len(targetParts) != 3 || targetParts[0] != p.ProjectID || targetParts[1] == "" || targetParts[2] == "" || !safeName(run) {
				return fmt.Errorf("%w: invalid publication route", ErrCorrupt)
			}
		}
	}
	return nil
}

// WritePublications requires LockPublicationRegistry. It never exposes plaintext passwords.
func (s *Store) WritePublications(reg PublicationRegistry) error {
	if e := validatePublications(reg); e != nil {
		return e
	}
	if e := s.ensure(); e != nil {
		return e
	}
	return s.write(filepath.Join(s.Root, "publications.json"), reg)
}
func (s *Store) WritePublicationRuntime(id string, record PublicationRuntime) error {
	if record.ID != id || !oneOf(record.State, "starting", "ready", "failed", "stopped") {
		return errors.New("invalid publication runtime")
	}
	dir, e := s.publicationDir(id, true)
	if e != nil {
		return e
	}
	return s.write(filepath.Join(dir, "runtime.json"), record)
}
func (s *Store) ReadPublicationRuntime(id string) (PublicationRuntime, error) {
	var record PublicationRuntime
	dir, e := s.publicationDir(id, false)
	if e != nil {
		return record, e
	}
	b, e := readFile(filepath.Join(dir, "runtime.json"))
	if e != nil {
		return record, e
	}
	if e = decode(b, &record); e != nil || record.ID != id || !oneOf(record.State, "starting", "ready", "failed", "stopped") {
		return PublicationRuntime{}, fmt.Errorf("%w: invalid publication runtime", ErrCorrupt)
	}
	return record, nil
}

// PublicationDir returns the private per-publication directory.
func (s *Store) PublicationDir(id string, create bool) (string, error) {
	return s.publicationDir(id, create)
}
