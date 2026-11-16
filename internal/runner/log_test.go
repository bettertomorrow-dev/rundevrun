// Tests runner log behavior.
package runner

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLogRotationBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	l, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("x"), logSegmentSize*3+17)
	if _, err = l.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"", ".1", ".2"} {
		st, err := os.Stat(path + n)
		if err != nil || st.Size() > logSegmentSize {
			t.Fatalf("segment %s: %v %v", n, st, err)
		}
	}
	if _, err = os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatal("too many segments")
	}
}
