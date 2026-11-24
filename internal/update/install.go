// Stages, verifies, and replaces the two binaries from a GitHub release archive.
package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

var binaries = [...]string{"rundevtop", "rundevrun"}

func install(ctx context.Context, base string, client *http.Client, dir, latest string, announce func()) error {
	if stat, err := os.Stat(dir); err != nil || !ownedByUser(stat) {
		return fmt.Errorf("updates require a user-owned installation directory: %s", dir)
	}
	stage := filepath.Join(dir, ".install-lock")
	if err := lockInstall(ctx, stage); err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	needed := false
	for _, name := range binaries {
		path := filepath.Join(dir, name)
		stat, err := os.Lstat(path)
		if err != nil || !stat.Mode().IsRegular() || !ownedByUser(stat) {
			return fmt.Errorf("expected a release binary at %s", path)
		}
		current, err := installedVersion(ctx, path, name)
		if err != nil {
			return err
		}
		if newer(current, latest) {
			return nil
		}
		needed = needed || newer(latest, current)
	}
	if !needed {
		return nil
	}

	announce()
	asset := fmt.Sprintf("rundevrun_%s_%s_%s.tar.gz", latest, runtime.GOOS, runtime.GOARCH)
	url := base + "/download/v" + latest + "/"
	sums, err := download(ctx, client, url+"checksums.txt", 64<<10)
	if err != nil {
		return err
	}
	var expected string
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			expected = fields[0]
			break
		}
	}
	if len(expected) != 64 {
		return fmt.Errorf("missing SHA-256 checksum for %s", asset)
	}
	archive, err := download(ctx, client, url+asset, 64<<20)
	if err != nil {
		return err
	}
	if actual := fmt.Sprintf("%x", sha256.Sum256(archive)); actual != expected {
		return fmt.Errorf("SHA-256 verification failed for %s", asset)
	}
	if err := extract(archive, stage); err != nil {
		return err
	}
	for _, name := range binaries {
		version, err := installedVersion(ctx, filepath.Join(stage, name), name)
		if err != nil {
			return err
		}
		if version != latest {
			return fmt.Errorf("archive contains %s %s, expected %s", name, version, latest)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return replace(stage, dir)
}

func ownedByUser(stat os.FileInfo) bool {
	return int(stat.Sys().(*syscall.Stat_t).Uid) == os.Geteuid() && stat.Mode().Perm()&0022 == 0
}

func lockInstall(ctx context.Context, stage string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		err := os.Mkdir(stage, 0700)
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("another install is active or left a lock at %s", stage)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func download(ctx context.Context, client *http.Client, url string, max int64) ([]byte, error) {
	secure := *client
	secure.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) >= 10 {
			return fmt.Errorf("unexpected release download redirect")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := secure.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s returned HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if int64(len(data)) > max {
		return nil, fmt.Errorf("release download too large: %s", url)
	}
	return data, err
}

func extract(archive []byte, stage string) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	defer gz.Close()
	reader := tar.NewReader(io.LimitReader(gz, 128<<20))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Name != "rundevrun" && header.Name != "rundevtop" {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 {
			return fmt.Errorf("invalid archive entry %s", header.Name)
		}
		file, err := os.OpenFile(filepath.Join(stage, header.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0755)
		if err != nil {
			return err
		}
		_, err = io.Copy(file, reader)
		if err := errors.Join(err, file.Close()); err != nil {
			return err
		}
	}
}

func replace(stage, dir string) error {
	for _, name := range binaries {
		if err := os.Link(filepath.Join(dir, name), filepath.Join(stage, name+".previous")); err != nil {
			return err
		}
	}
	for _, name := range binaries {
		if err := os.Rename(filepath.Join(stage, name+".previous"), filepath.Join(dir, name+".previous")); err != nil {
			return err
		}
	}
	for i, name := range binaries {
		if err := os.Rename(filepath.Join(stage, name), filepath.Join(dir, name)); err != nil {
			for _, replaced := range binaries[:i] {
				err = errors.Join(err, os.Rename(filepath.Join(dir, replaced+".previous"), filepath.Join(dir, replaced)))
			}
			return err
		}
	}
	return nil
}
