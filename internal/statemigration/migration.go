// Migrates the former runtime directory and removes its obsolete run data.
package statemigration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bettertomorrow-dev/rundevrun/internal/proxy"
	"github.com/bettertomorrow-dev/rundevrun/internal/runner"
	"github.com/bettertomorrow-dev/rundevrun/internal/state"
	"golang.org/x/sys/unix"
)

const Command = "config migrate-state"
const cleanupMarker = ".registrations-migrated"

var ErrRequired = errors.New("legacy runtime state needs migration. Run rundevrun " + Command + " to stop legacy processes and move their registrations")

func Prepare(ctx context.Context, current *state.Store, apply bool, progress io.Writer) error {
	legacy, err := legacyRoot(current.Root)
	if err != nil || legacy == "" {
		return err
	}
	if !apply {
		return ErrRequired
	}
	if err := migrate(ctx, current, legacy, progress); err != nil {
		return fmt.Errorf("legacy state migration failed: %w\nResolve the error, then run rundevrun %s.", err, Command)
	}
	return nil
}

func legacyRoot(current string) (string, error) {
	if os.Getenv("RDR_HOME") != "" {
		return "", nil
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return "", nil
		}
		base = filepath.Join(home, ".local", "state")
	}
	legacy, err := filepath.Abs(filepath.Join(base, "rundevrun"))
	if err != nil || legacy == current {
		return "", err
	}
	entries, err := os.ReadDir(legacy)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(entries) == 0) {
		return "", nil
	}
	return legacy, err
}

func migrate(ctx context.Context, current *state.Store, legacy string, progress io.Writer) error {
	parent, err := os.Open(filepath.Dir(legacy))
	if err != nil {
		return err
	}
	defer parent.Close()
	if err := unix.Flock(int(parent.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	legacy, err = legacyRoot(current.Root)
	if err != nil || legacy == "" {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(legacy)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("unsafe legacy state root %q", legacy)
	}
	source, err := filepath.EvalSymlinks(legacy)
	if err != nil {
		return err
	}
	target := current.Root
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	if source == target || strings.HasPrefix(target, source+string(filepath.Separator)) ||
		strings.HasPrefix(current.Root, legacy+string(filepath.Separator)) {
		return fmt.Errorf("runtime directories overlap: %q and %q", source, target)
	}

	marker := filepath.Join(legacy, cleanupMarker)
	if info, err := os.Lstat(marker); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != 0 {
			return fmt.Errorf("invalid migration cleanup marker %q", marker)
		}
		return removeLegacy(legacy)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	old := &state.Store{Root: legacy}
	fmt.Fprintln(progress, "Migrating legacy runtime state. Stopping old services, shares, and proxy. Old logs and run history will be deleted.")
	if err := stopLegacy(ctx, old); err != nil {
		return err
	}
	if err := importRegistrations(current, old); err != nil {
		return err
	}
	target, err = filepath.EvalSymlinks(current.Root)
	if err == nil && strings.HasPrefix(target, source+string(filepath.Separator)) {
		return fmt.Errorf("migration destination %q is inside legacy state %q", target, source)
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		return err
	}
	return removeLegacy(legacy)
}

func stopLegacy(ctx context.Context, old *state.Store) error {
	selections, _, err := runner.AllDownSelections(old)
	if err != nil {
		return err
	}
	publications, err := old.ReadPublications()
	if err != nil {
		return err
	}
	for _, id := range slices.Sorted(maps.Keys(publications.Publications)) {
		if publications.Publications[id].Phase != "closed" {
			if err := runner.ClosePublication(ctx, old, id); err != nil {
				return fmt.Errorf("stop legacy share %s: %w", id, err)
			}
		}
	}
	if len(selections) != 0 {
		if _, err := runner.New(old, "").DownGraph(ctx, selections, true); err != nil {
			return fmt.Errorf("stop legacy services: %w", err)
		}
	}
	if err := proxy.Stop(ctx, old); err != nil {
		return fmt.Errorf("stop legacy proxy: %w", err)
	}
	return ctx.Err()
}

func importRegistrations(current, old *state.Store) error {
	previous, err := old.Read()
	if err != nil || len(previous.Projects) == 0 {
		return err
	}
	return current.Update(func(reg *state.Registry) error {
		for _, id := range slices.Sorted(maps.Keys(previous.Projects)) {
			if _, exists := reg.Projects[id]; exists {
				continue
			}
			project, err := previous.Project(id)
			if err != nil {
				return err
			}
			for id, instance := range project.Instances {
				project.Instances[id] = state.Instance{
					ID: instance.ID, WorktreeID: instance.WorktreeID, Service: instance.Service,
					State: "stopped", Hostname: instance.Hostname, DesiredBindings: instance.DesiredBindings,
				}
			}
			if err := reg.SetProject(id, project); err != nil {
				return err
			}
		}
		return nil
	})
}

func removeLegacy(legacy string) error {
	root, err := os.OpenRoot(legacy)
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != cleanupMarker {
			if err := root.RemoveAll(entry.Name()); err != nil {
				return err
			}
		}
	}
	if err := root.Remove(cleanupMarker); err != nil {
		return err
	}
	return os.Remove(legacy)
}
