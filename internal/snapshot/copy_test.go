package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCopyAndBoundaries(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(src, "index.html")
	if err := os.WriteFile(file, []byte("original"), 0755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "snapshot")
	if err := Copy(context.Background(), root, "src", dest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "index.html"))
	if err != nil || string(data) != "original" {
		t.Fatalf("copy changed: %q %v", data, err)
	}
	info, _ := os.Stat(filepath.Join(dest, "index.html"))
	if info.Mode().Perm() != 0600 {
		t.Fatalf("file mode: %v", info.Mode())
	}
	if err := Copy(context.Background(), root, "src", dest); err == nil {
		t.Fatal("existing destination accepted")
	}
	if _, err := os.Stat(filepath.Join(dest, "index.html")); err != nil {
		t.Fatal("existing destination changed")
	}
	if err := Copy(context.Background(), root, "src", filepath.Join(src, "nested")); err == nil {
		t.Fatal("nested destination accepted")
	}
	if err := Copy(context.Background(), root, "../", filepath.Join(root, "outside")); err == nil {
		t.Fatal("source outside cwd accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := filepath.Join(root, "cancelled")
	if err := Copy(ctx, root, "src", cancelled); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := os.Stat(cancelled); !os.IsNotExist(err) {
		t.Fatal("cancelled copy created destination")
	}
}
func TestRejectLinksAndSpecials(t *testing.T) {
	for _, kind := range []string{"internal", "escaping", "cyclic", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "src")
			if err := os.Mkdir(src, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(src, "data"), []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(src, "link")
			var err error
			switch kind {
			case "internal":
				err = os.Symlink("data", target)
			case "escaping":
				err = os.Symlink("../secret", target)
			case "cyclic":
				err = os.Symlink("link", target)
			case "fifo":
				err = unix.Mkfifo(target, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(root, "dest")
			if err := Copy(context.Background(), root, "src", dest); err == nil {
				t.Fatal("unsafe artifact accepted")
			}
			if _, err := os.Stat(dest); !os.IsNotExist(err) {
				t.Fatal("failed copy left destination")
			}
			b, err := os.ReadFile(filepath.Join(src, "data"))
			if err != nil || string(b) != "keep" {
				t.Fatal("source changed")
			}
		})
	}
}

type cancellingContext struct {
	context.Context
	calls int
}

func (c *cancellingContext) Err() error {
	c.calls++
	if c.calls >= 3 {
		return context.Canceled
	}
	return nil
}
func TestCancellationRemovesOnlyNewDestination(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.Mkdir(src, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "file"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, "old")
	if err := os.Mkdir(old, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "keep"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "new")
	ctx := &cancellingContext{Context: context.Background()}
	if err := Copy(ctx, root, "src", dest); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("new destination remains")
	}
	if _, err := os.Stat(filepath.Join(old, "keep")); err != nil {
		t.Fatal("old publication changed")
	}
	if _, err := os.Stat(filepath.Join(src, "file")); err != nil {
		t.Fatal("source changed")
	}
}
