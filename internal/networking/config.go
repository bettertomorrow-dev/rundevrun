// Package networking stores the desired local and tailnet networking settings.
package networking

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/routing"
	"golang.org/x/sys/unix"
)

const ConfigVersion = 1

var ErrConflict = errors.New("network configuration changed since plan")

type Config struct {
	Version  int                `json:"version"`
	Local    Local              `json:"local"`
	Tailnet  Tailnet            `json:"tailnet"`
	Projects map[string]Project `json:"projects,omitempty"`
}
type Local struct {
	Listen string `json:"listen"`
}
type Tailnet struct {
	Domain        string `json:"domain,omitempty"`
	ZoneID        string `json:"zone_id,omitempty"`
	CredentialRef string `json:"credential_ref,omitempty"`
	Email         string `json:"email,omitempty"`
}
type Project struct {
	Tailnet         bool   `json:"tailnet"`
	DefaultService  string `json:"default_service,omitempty"`
	PrimaryWorktree string `json:"primary_worktree,omitempty"`
}
type Snapshot struct {
	Config   Config          `json:"config"`
	Revision string          `json:"revision"`
	Legacy   *routing.Config `json:"legacy,omitempty"`
}
type Plan struct {
	Baseline string   `json:"baseline"`
	Desired  Config   `json:"desired"`
	Changes  []string `json:"changes"`
}
type Service struct {
	Root       string
	cloudflare Cloudflare
	tailscale  tailscaleClient
	issuer     certificateIssuer
}

func DefaultConfig() Config {
	return Config{Version: ConfigVersion, Local: Local{Listen: "127.0.0.1:8080"}, Projects: map[string]Project{}}
}

func DefaultService() (Service, error) {
	path, err := routing.DefaultPath()
	return Service{Root: filepath.Dir(path)}, err
}

func (s Service) path() string { return filepath.Join(s.Root, "networking.json") }

// Inspect is read-only and does not contact external services or create files.
func (s Service) Inspect(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if s.Root == "" {
		return Snapshot{}, errors.New("empty configuration root")
	}
	c := DefaultConfig()
	b, err := readFile(s.path())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, err
	}
	if err == nil {
		var header struct {
			Version *int `json:"version"`
		}
		if err := json.Unmarshal(b, &header); err != nil || header.Version == nil {
			return Snapshot{}, errors.New("networking config requires a version")
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&c); err != nil {
			return Snapshot{}, fmt.Errorf("invalid networking config: %w", err)
		}
		if err = dec.Decode(new(any)); err != io.EOF {
			return Snapshot{}, errors.New("invalid trailing networking config data")
		}
		if err = validate(c); err != nil {
			return Snapshot{}, err
		}
	}
	legacyPath := filepath.Join(s.Root, "config.toml")
	var legacy *routing.Config
	if _, err := os.Lstat(legacyPath); err == nil {
		cfg, err := routing.Load(legacyPath)
		if err != nil {
			return Snapshot{}, err
		}
		legacy = &cfg
	} else if !errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, err
	}
	return Snapshot{Config: c, Revision: revision(b), Legacy: legacy}, nil
}

func revision(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

var refName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

func validate(c Config) error {
	if c.Version != ConfigVersion {
		return errors.New("unsupported networking config version")
	}
	host, port, err := net.SplitHostPort(c.Local.Listen)
	ip := net.ParseIP(host)
	n, e := strconv.Atoi(port)
	if err != nil || ip == nil || !ip.IsLoopback() || e != nil || n < 1024 || n > 65535 {
		return errors.New("local listen must be a loopback IP and unprivileged port")
	}
	if c.Tailnet.CredentialRef != "" && !refName.MatchString(c.Tailnet.CredentialRef) {
		return errors.New("credential_ref must be a private credential file name")
	}
	if c.Tailnet.Domain != "" {
		r := routing.Config{Routing: routing.Settings{Hostname: "{service}-{worktree}-{project}.{domain}"}}
		if _, err := r.Hostname("service", "worktree", "project", c.Tailnet.Domain); err != nil {
			return fmt.Errorf("invalid tailnet domain: %w", err)
		}
	}
	for id := range c.Projects {
		if strings.TrimSpace(id) == "" {
			return errors.New("empty project ID")
		}
	}
	return nil
}

// Plan computes desired local settings only. Provider operations are added by the
// provider implementation; a plan never carries secret material.
// PlanAtRevision rejects a stale draft before preparing its preview.
func (s Service) PlanAtRevision(ctx context.Context, desired Config, expected string) (Plan, error) {
	plan, err := s.Plan(ctx, desired)
	if err != nil {
		return Plan{}, err
	}
	if plan.Baseline != expected {
		return Plan{}, ErrConflict
	}
	return plan, nil
}

func (s Service) Plan(ctx context.Context, desired Config) (Plan, error) {
	if err := validate(desired); err != nil {
		return Plan{}, err
	}
	snap, err := s.Inspect(ctx)
	if err != nil {
		return Plan{}, err
	}
	changes := []string{}
	diff := func(name, before, after string) {
		if before != after {
			changes = append(changes, fmt.Sprintf("%s: %q -> %q", name, before, after))
		}
	}
	diff("local.listen", snap.Config.Local.Listen, desired.Local.Listen)
	diff("tailnet.domain", snap.Config.Tailnet.Domain, desired.Tailnet.Domain)
	diff("tailnet.zone_id", snap.Config.Tailnet.ZoneID, desired.Tailnet.ZoneID)
	diff("tailnet.credential_ref", snap.Config.Tailnet.CredentialRef, desired.Tailnet.CredentialRef)
	diff("tailnet.email", snap.Config.Tailnet.Email, desired.Tailnet.Email)
	ids := make(map[string]bool, len(snap.Config.Projects)+len(desired.Projects))
	for id := range snap.Config.Projects {
		ids[id] = true
	}
	for id := range desired.Projects {
		ids[id] = true
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	for _, id := range sorted {
		before, beforeOK := snap.Config.Projects[id]
		after, afterOK := desired.Projects[id]
		prefix := fmt.Sprintf("projects[%q]", id)
		if beforeOK != afterOK {
			diff(prefix+".configured", strconv.FormatBool(beforeOK), strconv.FormatBool(afterOK))
		}
		diff(prefix+".tailnet", strconv.FormatBool(before.Tailnet), strconv.FormatBool(after.Tailnet))
		diff(prefix+".default_service", before.DefaultService, after.DefaultService)
		diff(prefix+".primary_worktree", before.PrimaryWorktree, after.PrimaryWorktree)
	}

	return Plan{Baseline: snap.Revision, Desired: desired, Changes: changes}, nil
}

// Apply serializes writers and rejects stale plans. External changes are not
// performed here until the provider reconciler is integrated.
func (s Service) Apply(ctx context.Context, plan Plan) (Snapshot, error) {
	if err := validate(plan.Desired); err != nil {
		return Snapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if s.Root == "" {
		return Snapshot{}, errors.New("empty configuration root")
	}
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return Snapshot{}, err
	}
	info, err := os.Lstat(s.Root)
	if err != nil {
		return Snapshot{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return Snapshot{}, errors.New("configuration root must be a private directory")
	}
	f, err := os.OpenFile(filepath.Join(s.Root, "networking.lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return Snapshot{}, err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return Snapshot{}, fmt.Errorf("network configuration lock: %w", err)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	snap, err := s.Inspect(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if snap.Revision != plan.Baseline {
		return Snapshot{}, ErrConflict
	}
	b, err := json.MarshalIndent(plan.Desired, "", "  ")
	if err != nil {
		return Snapshot{}, err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(s.Root, ".networking-*")
	if err != nil {
		return Snapshot{}, err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Sync()
	}
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err != nil {
		return Snapshot{}, err
	}
	if err = ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	current, err := readFile(s.path())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, err
	}
	if revision(current) != plan.Baseline {
		return Snapshot{}, ErrConflict
	}
	if err = os.Rename(tmp.Name(), s.path()); err != nil {
		return Snapshot{}, err
	}
	dir, err := os.Open(s.Root)
	if err != nil {
		return Snapshot{}, err
	}
	err = dir.Sync()
	dir.Close()
	if err != nil {
		return Snapshot{}, err
	}
	return s.Inspect(ctx)
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
	if !st.Mode().IsRegular() {
		return nil, errors.New("networking config is not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err == nil && len(b) > 1<<20 {
		return nil, errors.New("networking config is too large")
	}
	return b, err
}
