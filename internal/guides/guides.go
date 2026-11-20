// Loads immutable guide revisions and checks the published guide snapshot.
package guides

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const protocol = 2
const maxArchive = 16 << 20
const maxGuides = 8 << 20

var names = []string{"onboard", "domain"}
var snapshotURL = "https://codeload.github.com/bettertomorrow-dev/rundevrun/zip/refs/heads/main"
var client = &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error { return errors.New("redirects are not allowed") }}

type Guide struct{ Name, Dir, Instructions, Source, Revision string }
type index struct {
	Checked   time.Time         `json:"checked"`
	Revisions map[string]string `json:"revisions"`
}
type manifest struct {
	Protocol int `json:"guide_protocol"`
	MinAPI   int `json:"min_cli_api_version"`
}

func Load(name string, noUpdate bool) (Guide, string, error) {
	var result Guide
	result.Name = name
	if !validName(name) {
		return result, "", fmt.Errorf("unknown guide %q", name)
	}
	config := os.Getenv("XDG_CONFIG_HOME")
	if config == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return result, "", err
		}
		config = filepath.Join(home, ".config")
	}
	root, err := filepath.Abs(filepath.Join(config, "rundevrun", "guides"))
	if err != nil {
		return result, "", err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return result, "", err
	}
	lock, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return result, "", err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return result, "", err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	idx := readIndex(root)
	warning := ""
	if !noUpdate && time.Since(idx.Checked) >= 24*time.Hour {
		revisions, updateErr := fetch(root)
		next := idx
		next.Checked = time.Now().UTC()
		if updateErr == nil {
			next.Revisions = revisions
		} else {
			warning = fmt.Sprintf("guide update failed: %v", updateErr)
		}
		if err := writeIndex(root, next); err != nil {
			if warning != "" {
				warning += "; "
			}
			warning += fmt.Sprintf("guide update state: %v", err)
		} else {
			idx = next
		}
	}
	local := filepath.Join(root, name+".local")
	if _, err = os.Lstat(local); err == nil {
		result.Dir = local
		data, revision, e := readDir(local)
		if e == nil {
			e = complete(name, data)
		}
		if e != nil {
			return result, warning, fmt.Errorf("invalid local guide %s: %w", local, e)
		}
		result.Instructions, result.Revision, result.Source = string(data["GUIDE.md"]), revision, "local"
		return result, warning, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		result.Dir = local
		return result, warning, err
	}
	if revision := idx.Revisions[name]; validRevision(revision) {
		dir := filepath.Join(root, "revisions", revision, name)
		data, actual, e := readDir(dir)
		if e == nil && actual == revision && complete(name, data) == nil {
			result.Dir, result.Instructions, result.Revision, result.Source = dir, string(data["GUIDE.md"]), revision, "downloaded"
			return result, warning, nil
		}
	}
	data, err := readFS(bundled, name)
	if err != nil {
		return result, warning, err
	}
	if err := complete(name, data); err != nil {
		return result, warning, err
	}
	revision := digest(data)
	dir, err := materialize(root, name, revision, data)
	if err != nil {
		return result, warning, err
	}
	result.Dir, result.Instructions, result.Revision, result.Source = dir, string(data["GUIDE.md"]), revision, "bundled"
	return result, warning, nil
}

func complete(name string, data map[string][]byte) error {
	if len(data["GUIDE.md"]) == 0 {
		return errors.New("missing or empty GUIDE.md")
	}
	if name == "domain" {
		for _, p := range []string{"references/cloudflare-onboarding.md", "references/prerequisites.md", "scripts/create-cloudflare-token.mjs"} {
			if len(data[p]) == 0 {
				return fmt.Errorf("missing or empty %s", p)
			}
		}
	}
	return nil
}
func Names() []string { return slices.Clone(names) }

func validName(name string) bool {
	for _, n := range names {
		if name == n {
			return true
		}
	}
	return false
}
func validRevision(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && s == strings.ToLower(s)
}
func digest(data map[string][]byte) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%d:%s:%d:", len(k), k, len(data[k]))
		h.Write(data[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}
func readFS(fsys fs.FS, root string) (map[string][]byte, error) {
	data := map[string][]byte{}
	total := 0
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("unsupported file %s", p)
		}
		file, err := fsys.Open(p)
		if err != nil {
			return err
		}
		b, err := io.ReadAll(io.LimitReader(file, int64(maxGuides-total)+1))
		file.Close()
		if err != nil {
			return err
		}
		total += len(b)
		if total > maxGuides {
			return errors.New("guide exceeds size limit")
		}
		data[strings.TrimPrefix(p, root+"/")] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(data["GUIDE.md"]) == 0 {
		return nil, errors.New("missing or empty GUIDE.md")
	}
	return data, nil
}
func readDir(dir string) (map[string][]byte, string, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, "", err
	}
	if !info.IsDir() {
		return nil, "", errors.New("guide is not a directory")
	}
	data, err := readFS(os.DirFS(dir), ".")
	if err != nil {
		return nil, "", err
	}
	return data, digest(data), nil
}
func materialize(root, name, revision string, data map[string][]byte) (string, error) {
	parent := filepath.Join(root, "revisions", revision)
	target := filepath.Join(parent, name)
	if old, actual, err := readDir(target); err == nil && actual == revision && len(old) > 0 {
		return target, nil
	}
	if err := os.MkdirAll(parent, 0700); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(parent, ".stage-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	for p, b := range data {
		dest := filepath.Join(stage, filepath.FromSlash(p))
		if err = os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return "", err
		}
		if err = os.WriteFile(dest, b, 0600); err != nil {
			return "", err
		}
	}
	if err = os.Rename(stage, target); err != nil {
		return "", err
	}
	return target, nil
}
func readIndex(root string) index {
	var idx index
	b, err := os.ReadFile(filepath.Join(root, fmt.Sprintf("index-v%d.json", protocol)))
	if err == nil {
		if json.Unmarshal(b, &idx) != nil {
			return index{}
		}
	}
	return idx
}
func writeIndex(root string, idx index) error {
	b, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(root, ".index-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(root, fmt.Sprintf("index-v%d.json", protocol)))
}
func fetch(root string) (map[string]string, error) {
	resp, err := client.Get(snapshotURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxArchive+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxArchive {
		return nil, errors.New("archive exceeds size limit")
	}
	z, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	seen := map[string]bool{}
	total := uint64(0)
	prefix := ""
	for _, f := range z.File {
		p := f.Name
		clean := path.Clean(p)
		if strings.Contains(p, "\\") || strings.HasPrefix(p, "/") || clean == ".." || strings.HasPrefix(clean, "../") || (clean != strings.TrimSuffix(p, "/")) {
			return nil, fmt.Errorf("unsafe archive path %q", p)
		}
		key := strings.ToLower(clean)
		if seen[key] {
			return nil, fmt.Errorf("duplicate archive path %q", p)
		}
		seen[key] = true
		if !f.FileInfo().IsDir() && !f.Mode().IsRegular() {
			return nil, fmt.Errorf("unsupported archive entry %q", p)
		}
		parts := strings.SplitN(clean, "/", 2)
		if len(parts) != 2 {
			continue
		}
		if prefix == "" {
			prefix = parts[0]
		}
		if parts[0] != prefix {
			return nil, errors.New("multiple archive roots")
		}
		rel := parts[1]
		if f.FileInfo().IsDir() {
			continue
		}
		wanted := rel == "internal/guides/manifest.json"
		for _, n := range names {
			wanted = wanted || strings.HasPrefix(rel, "internal/guides/"+n+"/")
		}
		if !wanted {
			continue
		}
		total += f.UncompressedSize64
		if total > maxGuides {
			return nil, errors.New("guides exceed size limit")
		}
		r, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(r, maxGuides+1))
		r.Close()
		if err != nil {
			return nil, err
		}
		if len(data) > maxGuides {
			return nil, errors.New("guide exceeds size limit")
		}
		files[rel] = data
	}
	var m manifest
	if err := json.Unmarshal(files["internal/guides/manifest.json"], &m); err != nil || m.Protocol != protocol || m.MinAPI < 1 || m.MinAPI > 1 {
		return nil, errors.New("incompatible guide manifest")
	}
	revisions := map[string]string{}
	for _, n := range names {
		data := map[string][]byte{}
		prefix := "internal/guides/" + n + "/"
		for p, b := range files {
			if strings.HasPrefix(p, prefix) {
				data[strings.TrimPrefix(p, prefix)] = b
			}
		}
		if err := complete(n, data); err != nil {
			return nil, fmt.Errorf("guide %s: %w", n, err)
		}
		rev := digest(data)
		if _, err = materialize(root, n, rev, data); err != nil {
			return nil, err
		}
		revisions[n] = rev
	}
	return revisions, nil
}
