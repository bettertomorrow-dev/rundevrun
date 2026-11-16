// Package monitor supplies read-only views and ownership-checked actions for rdrtop.
package monitor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/discovery"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
	"golang.org/x/sys/unix"
)

type Service struct {
	ID, ProjectID, ProjectName, WorktreeID, Worktree, Branch, Name, State, Protocol, RunID, URL, LocalURL, LocalAliasURL, TailnetURL, TailnetAliasURL, ReportedURL, DirectURL, Error string
	PID                                                                                                                                                                              int
	Ports                                                                                                                                                                            []int
	StartedAt                                                                                                                                                                        time.Time
	MissingWorktree                                                                                                                                                                  bool
	LaunchURLStale                                                                                                                                                                   bool
}
type Snapshot struct {
	Services         []Service
	CurrentProjectID string
	PreferErrors     bool
	Warnings         []string
}

func endpointURL(endpoint state.Endpoint) string {
	if endpoint.Hostname == "" || endpoint.Port < 1 {
		return ""
	}
	scheme := "http"
	if endpoint.Scope == "tailnet" {
		scheme = "https"
	}
	return scheme + "://" + endpoint.Hostname + ":" + strconv.Itoa(endpoint.Port)
}

func RuntimeRoot() (string, error) {
	if root := os.Getenv("RDR_HOME"); root != "" {
		return filepath.Abs(root)
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Abs(filepath.Join(base, "rundevrun"))
}

func contains(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func worktreePresence(ctx context.Context, p state.Project) (map[string]bool, []string) {
	present := map[string]bool{}
	var warnings []string
	var anchor string
	for _, w := range p.Worktrees {
		info, err := os.Stat(w.Root)
		switch {
		case errors.Is(err, os.ErrNotExist):
			present[w.ID] = false
		case err != nil:
			present[w.ID] = true
			warnings = append(warnings, fmt.Sprintf("%s: %v", w.Root, err))
		case !info.IsDir():
			present[w.ID] = false
		default:
			present[w.ID] = true
			if anchor == "" {
				anchor = w.Root
			}
		}
	}
	if p.GitCommonDir == "" || anchor == "" {
		return present, warnings
	}
	cmd := exec.CommandContext(ctx, "git", "-c", "core.fsmonitor=false", "worktree", "list", "--porcelain", "-z")
	cmd.Dir = anchor
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return present, append(warnings, fmt.Sprintf("%s: cannot inspect Git worktrees: %v", p.Name, err))
	}
	roots := map[string]bool{}
	for _, field := range strings.Split(string(out), "\x00") {
		if strings.HasPrefix(field, "worktree ") {
			path := strings.TrimPrefix(field, "worktree ")
			if canonical, err := filepath.EvalSymlinks(path); err == nil {
				path = canonical
			}
			roots[filepath.Clean(path)] = true
		}
	}
	for _, w := range p.Worktrees {
		if !present[w.ID] {
			continue
		}
		path, err := filepath.EvalSymlinks(w.Root)
		if err != nil {
			continue
		}
		if !roots[filepath.Clean(path)] {
			present[w.ID] = false
		}
	}
	return present, warnings
}

func Read(ctx context.Context, store *state.Store, cwd string) (Snapshot, error) {
	var out Snapshot
	if store == nil {
		return out, errors.New("nil state store")
	}
	reg, err := store.Read()
	if err != nil {
		return out, err
	}
	history, err := store.ReadLaunchHistory()
	if err != nil {
		return out, err
	}
	if canonical, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = canonical
	}
	depth := -1
	var projects []state.Project
	for id := range reg.Projects {
		p, err := reg.Project(id)
		if err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			continue
		}
		projects = append(projects, p)
		for _, w := range p.Worktrees {
			if contains(w.Root, cwd) && len(w.Root) > depth {
				out.CurrentProjectID, depth = p.ID, len(w.Root)
			}
		}
	}
	if out.CurrentProjectID == "" {

		if p, err := discovery.Discover(cwd); err == nil {
			if _, ok := reg.Projects[p.ID]; ok {
				out.CurrentProjectID = p.ID
			}
		}
	}
	var latest state.LaunchResult
	for projectID, attempt := range history.Projects {
		if out.CurrentProjectID != "" && projectID != out.CurrentProjectID {
			continue
		}
		if attempt.StartedAt.After(latest.StartedAt) {
			latest = attempt
		}
	}
	out.PreferErrors = latest.Error != ""
	for _, p := range projects {
		present, warnings := worktreePresence(ctx, p)
		out.Warnings = append(out.Warnings, warnings...)
		worktrees := map[string]state.Worktree{}
		for _, w := range p.Worktrees {
			worktrees[w.ID] = w
		}
		for id, inst := range p.Instances {
			w := worktrees[inst.WorktreeID]
			svc := Service{ID: id, ProjectID: p.ID, ProjectName: p.Name, WorktreeID: w.ID, Worktree: w.Root, Branch: w.Branch, Name: inst.Service, RunID: inst.RunID, MissingWorktree: !present[w.ID]}
			observed, inspectErr := store.Inspect(inst)
			if inspectErr != nil {
				svc.State = "unknown"
				out.Warnings = append(out.Warnings, fmt.Sprintf("%s: %v", id, inspectErr))
			} else {
				svc.State = observed
			}
			live := svc.State == "starting" || svc.State == "running" || svc.State == "ready" || svc.State == "unhealthy" || svc.State == "unknown"
			if attempt, ok := history.Services[id]; ok && attempt.Error != "" && present[w.ID] {
				svc.Error = attempt.Error
			}
			if !live && svc.Error == "" {
				continue
			}
			if svc.RunID != "" {
				manifest, err := store.ReadManifest(svc.RunID)
				if err != nil {
					out.Warnings = append(out.Warnings, fmt.Sprintf("%s: %v", id, err))
				} else if manifest.InstanceID != id {
					out.Warnings = append(out.Warnings, id+": manifest instance mismatch")
				} else {
					svc.DirectURL, svc.URL = manifest.Addresses["direct_url"], manifest.Addresses["url"]
					svc.Protocol = manifest.Launch.Protocol
					var localURL string
					for _, endpoint := range inst.Endpoints {
						u := endpointURL(endpoint)
						switch endpoint.Scope {
						case "local":
							if endpoint.Alias {
								svc.LocalAliasURL = u
							} else {
								localURL = u
							}
						case "tailnet":
							if endpoint.Alias {
								svc.TailnetAliasURL = u
							} else {
								svc.TailnetURL = u
							}
						}
					}
					svc.LocalURL = localURL
					svc.LaunchURLStale = localURL != "" && manifest.Addresses["url"] != localURL
					if inst.RouteURL != "" {
						svc.URL = inst.RouteURL
					}
					if reported, err := runner.New(store, "").ReportedURL(ctx, svc.RunID); err != nil {
						out.Warnings = append(out.Warnings, fmt.Sprintf("%s: cannot read reported URL: %v", id, err))
					} else if reported != "" {
						transferred := false
						for _, destination := range []*string{&svc.LocalURL, &svc.TailnetURL, &svc.LocalAliasURL, &svc.TailnetAliasURL} {
							if *destination == "" {
								continue
							}
							if value, ok := runner.TransferOwnedReportedURL(store, p.ID, inst, *destination, reported); ok {
								*destination = value
								transferred = true
							}
						}
						if !transferred {
							svc.ReportedURL = reported
							if localURL == "" {
								svc.URL = reported
							}
						}
					}
					if svc.LocalURL != "" {
						svc.URL = svc.LocalURL
					}
					if port := manifest.Launch.Port; port > 0 {
						svc.Ports = []int{port}
					}
					if info, err := os.Stat(filepath.Join(store.Root, "runs", svc.RunID, "manifest.json")); err == nil {
						svc.StartedAt = info.ModTime()
					}
				}
				if run, err := store.ReadRun(svc.RunID); err == nil {
					svc.PID = run.Child.PID
				}
			}
			out.Services = append(out.Services, svc)
		}
	}
	sort.Slice(out.Services, func(i, j int) bool {
		a, b := out.Services[i], out.Services[j]
		if a.ProjectName != b.ProjectName {
			return a.ProjectName < b.ProjectName
		}
		if a.ProjectID != b.ProjectID {
			return a.ProjectID < b.ProjectID
		}
		if a.Worktree != b.Worktree {
			return a.Worktree < b.Worktree
		}
		return a.Name < b.Name
	})
	sort.Strings(out.Warnings)
	if out.PreferErrors {
		out.PreferErrors = false
		for _, svc := range out.Services {
			if out.CurrentProjectID != "" && svc.ProjectID != out.CurrentProjectID {
				continue
			}
			if svc.Error != "" && history.Services[svc.ID].Attempt == latest.Attempt {
				out.PreferErrors = true
				break
			}
		}
	}
	return out, nil
}

func selection(store *state.Store, s Service) (runner.Selection, state.Instance, error) {
	reg, err := store.Read()
	if err != nil {
		return runner.Selection{}, state.Instance{}, err
	}
	p, err := reg.Project(s.ProjectID)
	if err != nil {
		return runner.Selection{}, state.Instance{}, err
	}
	i, ok := p.Instances[s.ID]
	if !ok || i.ID != s.ID || i.Service != s.Name || i.WorktreeID != s.WorktreeID || i.RunID != s.RunID {
		return runner.Selection{}, state.Instance{}, errors.New("service or run changed; refresh and retry")
	}
	for _, w := range p.Worktrees {
		if w.ID == i.WorktreeID {
			return runner.Selection{Project: &discovery.Project{ID: p.ID, Name: p.Name, Root: p.Root, GitCommonDir: p.GitCommonDir}, Worktree: &discovery.Worktree{ID: w.ID, Root: w.Root, Branch: w.Branch, Detached: w.Detached}, Service: i.Service}, i, nil
		}
	}
	return runner.Selection{}, state.Instance{}, errors.New("worktree descriptor missing")
}

func Stop(ctx context.Context, store *state.Store, s Service) error {
	sel, instance, err := selection(store, s)
	if err != nil {
		return err
	}
	if instance.RunID == "" {
		return errors.New("service has no recorded run")
	}
	r := runner.New(store, "")
	r.ExpectedRuns = map[string]string{s.ID: s.RunID}

	if _, err := os.Stat(sel.Worktree.Root); errors.Is(err, os.ErrNotExist) {
		r.SkipStopHooks = true
	}
	_, err = r.DownGraph(ctx, []runner.Selection{sel}, false)
	return err
}

func Logs(ctx context.Context, store *state.Store, s Service, tail int) (string, error) {
	if tail < 0 || tail > 10000 {
		return "", errors.New("invalid log line count")
	}
	sel, _, err := selection(store, s)
	if err != nil {
		return "", err
	}
	if s.Error != "" {
		if path := failedLogPath(store, s.ID, s.Error); path != "" {
			return tailLog(path, tail)
		}
	}
	var out bytes.Buffer
	err = runner.New(store, "").Logs(ctx, sel.Project, sel.Worktree, sel.Service, tail, false, &out)
	return out.String(), err
}

func failedLogPath(store *state.Store, id, failure string) string {
	dir := filepath.Join(store.Root, "runs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	history, err := store.ReadLaunchHistory()
	if err != nil {
		return ""
	}
	attempt := history.Services[id]
	var newest time.Time
	var result string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		m, err := store.ReadManifest(entry.Name())
		if err != nil || m.InstanceID != id {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, entry.Name(), "manifest.json"))
		if err != nil || info.ModTime().Before(attempt.StartedAt) || !info.ModTime().After(newest) {
			continue
		}
		paths := []string{"application.log", "build.log"}
		if strings.HasPrefix(failure, "BUILD_FAILED:") {
			paths = []string{"build.log", "application.log"}
		}
		for _, name := range paths {
			path := filepath.Join(dir, entry.Name(), name)
			if _, err := os.Lstat(path); err == nil {
				newest, result = info.ModTime(), path
				break
			}
		}
	}
	return result
}

func tailLog(path string, count int) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return "", errors.New("unsafe log file")
	}
	lines := make([]string, 0, count)
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 4096), 1<<20)
	for scan.Scan() {
		if count > 0 {
			if len(lines) == count {
				copy(lines, lines[1:])
				lines = lines[:count-1]
			}
			lines = append(lines, scan.Text())
		}
	}
	if err := scan.Err(); err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "", nil
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func PortLabel(ports []int) string {
	var values []string
	for _, p := range ports {
		values = append(values, strconv.Itoa(p))
	}
	return strings.Join(values, ",")
}
