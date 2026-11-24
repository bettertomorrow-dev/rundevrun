// Checks GitHub releases and updates installed release binaries before startup.
package update

import (
	"context"
	"crypto/sha256"
	"fmt"
	"go/version"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	releases      = "https://github.com/bettertomorrow-dev/rundevrun/releases"
	cask          = "bettertomorrow-dev/tap/rundevrun"
	checkInterval = 24 * time.Hour
)

var stableVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type Info struct {
	Current   string `json:"current_version"`
	Latest    string `json:"latest_version"`
	Available bool   `json:"update_available"`
}

func (i Info) String() string {
	return fmt.Sprintf("Installed: %s\nLatest stable release: %s", i.Current, i.Latest)
}

func newer(candidate, current string) bool {
	return version.Compare("go"+candidate, "go"+current) > 0
}

func Check(ctx context.Context, current string) (Info, error) {
	info, err := check(ctx, releases, http.DefaultClient, current)
	if err == nil && stableVersion.MatchString(current) {
		path, pathErr := executable()
		if pathErr == nil {
			cache, _ := os.UserCacheDir()
			cacheRelease(cachePath(cache, filepath.Dir(path)), info.Latest)
		}
	}
	return info, err
}

func check(ctx context.Context, base string, client *http.Client, current string) (Info, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	redirect := *client
	redirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, base+"/latest", nil)
	if err != nil {
		return Info{}, err
	}
	resp, err := redirect.Do(req)
	if err != nil {
		return Info{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusMovedPermanently && resp.StatusCode != http.StatusTemporaryRedirect && resp.StatusCode != http.StatusPermanentRedirect {
		return Info{}, fmt.Errorf("GitHub release check returned HTTP %d", resp.StatusCode)
	}
	location, err := resp.Location()
	if err != nil {
		return Info{}, err
	}
	latest, ok := strings.CutPrefix(location.String(), base+"/tag/v")
	if !ok || !stableVersion.MatchString(latest) {
		return Info{}, fmt.Errorf("GitHub did not return a stable release tag")
	}
	return Info{Current: current, Latest: latest, Available: newer(latest, current)}, nil
}

func Startup(ctx context.Context, current string, stderr io.Writer) {
	if !stableVersion.MatchString(current) {
		return
	}
	path, err := executable()
	if err != nil {
		fmt.Fprintln(stderr, "rundevrun update:", err)
		return
	}
	cache, _ := os.UserCacheDir()
	next, latest, err := ensure(ctx, releases, http.DefaultClient, cache, path, current, stderr)
	if next != "" && ctx.Err() == nil {
		fmt.Fprintf(stderr, "rundevrun updated to %s\n", latest)
		err = unix.Exec(next, os.Args, os.Environ())
	}
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(stderr, "rundevrun update:", err)
	}
}

func executable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(path)
}

func ensure(ctx context.Context, base string, client *http.Client, cache, path, current string, stderr io.Writer) (string, string, error) {
	dir, name := filepath.Dir(path), filepath.Base(path)
	if name != "rundevrun" && name != "rundevtop" {
		return "", "", nil
	}
	prefix := brewPrefix(dir)
	stamp := cachePath(cache, dir)
	latest := cachedRelease(stamp)
	if latest == "" {
		info, err := check(ctx, base, client, current)
		if err != nil {
			return "", "", err
		}
		latest = info.Latest
		cacheRelease(stamp, latest)
	}
	if !newer(latest, current) {
		return "", "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	announce := func() { fmt.Fprintf(stderr, "rundevrun %s is available. Installing update...\n", latest) }
	if prefix != "" {
		path = filepath.Join(prefix, "bin", name)
		installed, _ := installedVersion(ctx, path, name)
		if newer(latest, installed) {
			announce()
			cmd := exec.CommandContext(ctx, filepath.Join(prefix, "bin", "brew"), "upgrade", "--cask", cask)
			cmd.Env = append(os.Environ(), "HOMEBREW_NO_ASK=1", "HOMEBREW_NO_INSTALL_CLEANUP=1")
			cmd.WaitDelay = time.Second
			if output, err := cmd.CombinedOutput(); err != nil {
				return "", "", fmt.Errorf("Homebrew upgrade: %w: %s", err, strings.TrimSpace(string(output)))
			}
		}
	} else if err := install(ctx, base, client, dir, latest, announce); err != nil {
		return "", "", err
	}
	installed, err := installedVersion(ctx, path, name)
	if err != nil {
		return "", "", err
	}
	if newer(latest, installed) {
		return "", "", fmt.Errorf("release %s is available, but %s still reports %s", latest, path, installed)
	}
	return path, installed, nil
}

func cachePath(cache, dir string) string {
	if cache == "" {
		return ""
	}
	cache = filepath.Join(cache, "rundevrun", "updates")
	if os.MkdirAll(cache, 0700) != nil {
		return ""
	}
	if prefix := brewPrefix(dir); prefix != "" {
		dir = prefix
	}
	return filepath.Join(cache, fmt.Sprintf("%x", sha256.Sum256([]byte(dir))))
}

func brewPrefix(dir string) string {
	caskDir := filepath.Dir(dir)
	room := filepath.Dir(caskDir)
	if filepath.Base(caskDir) == "rundevrun" && filepath.Base(room) == "Caskroom" && stableVersion.MatchString(filepath.Base(dir)) {
		return filepath.Dir(room)
	}
	return ""
}

func cachedRelease(path string) string {
	stat, err := os.Stat(path)
	if err != nil || stat.Size() > 128 {
		return ""
	}
	age := time.Since(stat.ModTime())
	if age < 0 || age >= checkInterval {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil || !stableVersion.Match(data) {
		return ""
	}
	return string(data)
}

func cacheRelease(path, latest string) {
	if path == "" {
		return
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".release-*")
	if err != nil {
		return
	}
	defer os.Remove(file.Name())
	_, writeErr := io.WriteString(file, latest)
	if file.Close() == nil && writeErr == nil {
		_ = os.Rename(file.Name(), path)
	}
}

func installedVersion(ctx context.Context, path, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return "", fmt.Errorf("read %s version: %w", name, err)
	}
	fields := strings.Fields(string(output))
	if len(fields) < 2 || fields[0] != name || !stableVersion.MatchString(fields[1]) {
		return "", fmt.Errorf("expected a stable release binary at %s", path)
	}
	return fields[1], nil
}
