// Exercises archive and Homebrew upgrades with local release fixtures.
package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAutomaticArchiveUpdate(t *testing.T) {
	const old, latest = "0.9.0", "0.10.0"
	dir, cache := t.TempDir(), t.TempDir()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tarball := tar.NewWriter(gz)
	for _, name := range binaries {
		writeFixture(t, filepath.Join(dir, name), versionScript(name, old))
		body := versionScript(name, latest)
		if err := tarball.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarball.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarball.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	asset := fmt.Sprintf("rundevrun_%s_%s_%s.tar.gz", latest, runtime.GOOS, runtime.GOARCH)
	var checks, downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			checks.Add(1)
			http.Redirect(w, r, "/tag/v"+latest, http.StatusFound)
		case "/download/v" + latest + "/checksums.txt":
			fmt.Fprintf(w, "%x  %s\n", sha256.Sum256(archive.Bytes()), asset)
		case "/download/v" + latest + "/" + asset:
			if downloads.Add(1) == 1 {
				fmt.Fprint(w, "corrupt archive")
				return
			}
			_, _ = w.Write(archive.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	path := filepath.Join(dir, "rundevrun")
	var progress bytes.Buffer
	next, _, err := ensure(t.Context(), server.URL, server.Client(), cache, path, old, &progress)
	if next != "" || err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("corrupt archive: next=%q, err=%v", next, err)
	}
	if got := progress.String(); got != "rundevrun 0.10.0 is available. Installing update...\n" {
		t.Fatalf("install progress: %q", got)
	}
	for _, name := range binaries {
		if version, err := installedVersion(t.Context(), filepath.Join(dir, name), name); err != nil || version != old {
			t.Fatalf("%s after corrupt update: version=%q, err=%v", name, version, err)
		}
	}
	for _, name := range binaries {
		path := filepath.Join(dir, name)
		next, version, err := ensure(t.Context(), server.URL, server.Client(), cache, path, old, io.Discard)
		if next != path || version != latest || err != nil {
			t.Fatalf("update from %s: next=%q, version=%q, err=%v", name, next, version, err)
		}
		if version, err := installedVersion(t.Context(), path+".previous", name); err != nil || version != old {
			t.Fatalf("%s backup: version=%q, err=%v", name, version, err)
		}
	}
	progress.Reset()
	if next, _, err := ensure(t.Context(), server.URL, server.Client(), cache, path, latest, &progress); next != "" || err != nil || progress.Len() != 0 {
		t.Fatalf("already current: next=%q, err=%v", next, err)
	}
	if checks.Load() != 1 || downloads.Load() != 2 {
		t.Fatalf("checks=%d, downloads=%d", checks.Load(), downloads.Load())
	}
}

func TestAutomaticHomebrewUpdate(t *testing.T) {
	prefix := t.TempDir()
	oldDir := filepath.Join(prefix, "Caskroom", "rundevrun", "0.9.0")
	newDir := filepath.Join(prefix, "Caskroom", "rundevrun", "0.10.0")
	bin := filepath.Join(prefix, "bin")
	for _, dir := range []string{oldDir, newDir, bin} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range binaries {
		writeFixture(t, filepath.Join(oldDir, name), versionScript(name, "0.9.0"))
		writeFixture(t, filepath.Join(newDir, name), versionScript(name, "0.10.0"))
		if err := os.Symlink(filepath.Join(oldDir, name), filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(t, filepath.Join(bin, "brew"), fmt.Sprintf(`#!/bin/sh
set -eu
test "$*" = 'upgrade --cask bettertomorrow-dev/tap/rundevrun'
for name in rundevrun rundevtop; do
    ln -sfn '%s'/"$name" '%s'/"$name"
done
`, newDir, bin))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/tag/v0.10.0", http.StatusFound)
	}))
	defer server.Close()
	next, version, err := ensure(t.Context(), server.URL, server.Client(), t.TempDir(), filepath.Join(oldDir, "rundevtop"), "0.9.0", io.Discard)
	if next != filepath.Join(bin, "rundevtop") || version != "0.10.0" || err != nil {
		t.Fatalf("Homebrew update: next=%q, version=%q, err=%v", next, version, err)
	}
}

func versionScript(name, version string) string {
	return fmt.Sprintf("#!/bin/sh\necho '%s %s'\n", name, version)
}

func writeFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0755); err != nil {
		t.Fatal(err)
	}
}
