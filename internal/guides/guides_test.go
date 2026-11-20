// Tests guide selection, updates, and concurrent readers.
package guides

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var version atomic.Int32
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var b bytes.Buffer
		z := zip.NewWriter(&b)
		publishedProtocol := protocol
		if version.Load() == 2 {
			publishedProtocol++
		}
		files := map[string]string{"internal/guides/manifest.json": fmt.Sprintf(`{"guide_protocol":%d,"min_cli_api_version":1}`, publishedProtocol)}
		for _, n := range names {
			files["internal/guides/"+n+"/GUIDE.md"] = fmt.Sprintf("%s %d", n, version.Load())
		}
		for _, p := range []string{"references/cloudflare-onboarding.md", "references/prerequisites.md", "scripts/create-cloudflare-token.mjs"} {
			files["internal/guides/domain/"+p] = "content"
		}
		if version.Load() == 3 {
			files["../escape"] = "bad"
		}
		for name, content := range files {
			f, err := z.Create("repo/" + name)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = f.Write([]byte(content))
		}
		_ = z.Close()
		_, _ = w.Write(b.Bytes())
	}))
	defer server.Close()
	oldURL := snapshotURL
	snapshotURL = server.URL
	defer func() { snapshotURL = oldURL }()
	bundledGuide, _, err := Load("onboard", true)
	if err != nil || bundledGuide.Source != "bundled" || calls.Load() != 0 {
		t.Fatalf("bundled: %+v %v", bundledGuide, err)
	}
	root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "rundevrun", "guides")
	local := filepath.Join(root, "onboard.local")
	if err = os.Mkdir(local, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(local, "GUIDE.md"), []byte("local"), 0600); err != nil {
		t.Fatal(err)
	}
	g, warning, err := Load("onboard", false)
	if err != nil || warning != "" || g.Source != "local" || calls.Load() != 1 {
		t.Fatalf("override: %+v %q %v", g, warning, err)
	}
	if err = os.RemoveAll(local); err != nil {
		t.Fatal(err)
	}
	first, _, err := Load("onboard", true)
	if err != nil || first.Source != "downloaded" || first.Instructions != "onboard 0" {
		t.Fatalf("download: %+v %v", first, err)
	}
	version.Store(1)
	if err = os.Remove(filepath.Join(root, fmt.Sprintf("index-v%d.json", protocol))); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, _, e := Load("onboard", false)
			if e != nil || g.Instructions != "onboard 1" {
				t.Errorf("concurrent: %+v %v", g, e)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 2 {
		t.Fatalf("checks: %d", calls.Load())
	}
	if _, err = os.Stat(filepath.Join(first.Dir, "GUIDE.md")); err != nil {
		t.Fatal(err)
	}
	version.Store(2)
	idx := readIndex(root)
	idx.Checked = idx.Checked.Add(-25 * 60 * 60 * 1e9)
	if err = writeIndex(root, idx); err != nil {
		t.Fatal(err)
	}
	g, warning, err = Load("onboard", false)
	if err != nil || warning == "" || g.Instructions != "onboard 1" {
		t.Fatalf("fallback: %+v %q %v", g, warning, err)
	}
	version.Store(3)
	idx = readIndex(root)
	idx.Checked = idx.Checked.Add(-25 * time.Hour)
	if err = writeIndex(root, idx); err != nil {
		t.Fatal(err)
	}
	g, warning, err = Load("onboard", false)
	if err != nil || warning == "" || g.Instructions != "onboard 1" {
		t.Fatalf("unsafe archive: %+v %q %v", g, warning, err)
	}
	version.Store(4)
	idx = readIndex(root)
	idx.Checked = idx.Checked.Add(-25 * time.Hour)
	if err = writeIndex(root, idx); err != nil {
		t.Fatal(err)
	}
	g, warning, err = Load("onboard", false)
	if err != nil || warning != "" || g.Instructions != "onboard 4" {
		t.Fatalf("recovery: %+v %q %v", g, warning, err)
	}
	if err = os.Mkdir(local, 0700); err != nil {
		t.Fatal(err)
	}
	g, _, err = Load("onboard", true)
	if err == nil || g.Dir != local || !strings.Contains(err.Error(), local) {
		t.Fatalf("invalid override: %+v %v", g, err)
	}
	filename := filepath.Join(local, "GUIDE.md")
	if err = os.WriteFile(filename, []byte("local"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Truncate(filename, maxGuides+1); err != nil {
		t.Fatal(err)
	}
	g, _, err = Load("onboard", true)
	if err == nil || g.Dir != local || !strings.Contains(err.Error(), local) || !strings.Contains(err.Error(), "exceeds size limit") {
		t.Fatalf("oversized override: %+v %v", g, err)
	}
}
