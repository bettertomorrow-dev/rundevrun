// ReportedURL returns the latest explicit "app: url=..." announcement in this run's retained application logs. Ordinary URLs in log messages are ignored.
package runner

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/url"
	"strconv"

	"github.com/bettertomorrow-dev/rundevrun/internal/state"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

func (r *Runner) ReportedURL(ctx context.Context, runID string) (string, error) {

	if _, err := r.Store.ReadManifest(runID); err != nil {
		return "", err
	}
	var reported string

	for _, suffix := range []string{".2", ".1", ""} {
		f, err := openReadLog(filepath.Join(r.Store.Root, "runs", runID, "application.log") + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		scan := bufio.NewScanner(f)
		scan.Buffer(make([]byte, 4096), logSegmentSize+1)
		for scan.Scan() {
			if err := ctx.Err(); err != nil {
				f.Close()
				return "", err
			}
			prefix, value, ok := strings.Cut(scan.Text(), ": url=")
			if !ok || strings.TrimSpace(prefix) == "" {
				continue
			}
			value = strings.TrimSpace(value)
			u, err := url.Parse(value)
			if err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && strings.IndexFunc(value, unicode.IsSpace) < 0 {
				reported = value
			}
		}
		err = scan.Err()
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return "", errors.Join(err, closeErr)
		}
	}
	return reported, nil
}

func TransferOwnedReportedURL(store *state.Store, projectID string, instance state.Instance, destination, reported string) (string, bool) {
	if destination == "" || reported == "" || instance.RunID == "" {
		return destination, false
	}
	manifest, err := store.ReadManifest(instance.RunID)
	if err != nil || manifest.InstanceID != instance.ID || manifest.Launch.Protocol != "http" {
		return destination, false
	}
	port, err := VerifyProxyTarget(store, projectID, instance.ID, instance.RunID)
	if err != nil {
		return destination, false
	}
	direct, err := url.Parse(manifest.Addresses["direct_url"])
	if err != nil || direct.Scheme != "http" || direct.User != nil || direct.Port() != strconv.Itoa(port) {
		return destination, false
	}
	ip := net.ParseIP(direct.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return destination, false
	}
	source, err := url.Parse(reported)
	if err != nil || source.User != nil || source.Scheme != direct.Scheme || !strings.EqualFold(source.Host, direct.Host) {
		return destination, false
	}
	target, err := url.Parse(destination)
	if err != nil || target.Host == "" || target.User != nil || (target.Scheme != "http" && target.Scheme != "https") {
		return destination, false
	}
	target.Path, target.RawPath = source.Path, source.RawPath
	target.RawQuery, target.ForceQuery, target.Fragment, target.RawFragment = source.RawQuery, source.ForceQuery, source.Fragment, source.RawFragment
	return target.String(), true
}
