// Package snapshot copies a build artifact into a private publication directory.
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Copy creates a private independent copy. The artifact tree may not contain symlinks.
func Copy(ctx context.Context, cwd, artifactDir, destination string) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	if artifactDir == "" || filepath.IsAbs(artifactDir) {
		return errors.New("artifact directory must be relative to cwd")
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return err
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return err
	}
	source, err := filepath.EvalSymlinks(filepath.Join(cwd, artifactDir))
	if err != nil {
		return err
	}
	source, err = filepath.Abs(source)
	if err != nil {
		return err
	}
	if !inside(cwd, source) {
		return errors.New("artifact directory escapes cwd")
	}
	dest, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(dest))
	if err != nil {
		return err
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		return err
	}
	dest = filepath.Join(parent, filepath.Base(dest))
	if inside(source, dest) || inside(dest, source) {
		return errors.New("destination overlaps artifact directory")
	}
	cwdRoot, err := os.OpenRoot(cwd)
	if err != nil {
		return err
	}
	defer cwdRoot.Close()
	rel, err := filepath.Rel(cwd, source)
	if err != nil {
		return err
	}
	sourceRoot, err := cwdRoot.OpenRoot(rel)
	if err != nil {
		return err
	}
	defer sourceRoot.Close()
	dir, err := sourceRoot.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	if err = os.Mkdir(dest, 0700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dest)
		}
	}()
	err = copyDir(ctx, dir, dest)
	return err
}
func copyDir(ctx context.Context, src *os.File, dest string) error {
	entries, err := src.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifact contains symlink: %s", entry.Name())
		}
		// Nested files come from descriptor-relative Openat. DirEntry.Info
		// would resolve their display name against the process working directory.
		if kind := entry.Type(); kind != 0 && !kind.IsDir() {
			return fmt.Errorf("artifact contains unsupported file: %s", entry.Name())
		}
		fd, e := unix.Openat(int(src.Fd()), entry.Name(), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if e != nil {
			return fmt.Errorf("open artifact %s: %w", entry.Name(), e)
		}
		f := os.NewFile(uintptr(fd), entry.Name())
		info, e := f.Stat()
		if e != nil {
			f.Close()
			return e
		}
		target := filepath.Join(dest, entry.Name())
		switch {
		case info.IsDir():
			e = os.Mkdir(target, 0700)
			if e == nil {
				e = copyDir(ctx, f, target)
			}
		case info.Mode().IsRegular():
			var out *os.File
			out, e = os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if e == nil {
				_, e = io.Copy(out, contextReader{ctx, f})
				closeErr := out.Close()
				if e == nil {
					e = closeErr
				}
			}
		default:
			e = fmt.Errorf("artifact contains unsupported file: %s", entry.Name())
		}
		closeErr := f.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return ctx.Err()
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
