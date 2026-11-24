// Tests runtime directory selection without initializing storage.
package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeRoot(t *testing.T) {
	base := t.TempDir()
	for _, tc := range []struct {
		name, override, config, want string
	}{
		{"home", "", "", filepath.Join(base, ".config", "rundevrun", "state")},
		{"config", "", filepath.Join(base, "config"), filepath.Join(base, "config", "rundevrun", "state")},
		{"override", filepath.Join(base, "custom"), filepath.Join(base, "config"), filepath.Join(base, "custom")},
		{"relative", "relative-state", "", "relative-state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", base)
			t.Setenv("RDR_HOME", tc.override)
			t.Setenv("XDG_CONFIG_HOME", tc.config)
			t.Setenv("XDG_STATE_HOME", filepath.Join(base, "old"))
			want, err := filepath.Abs(tc.want)
			if err != nil {
				t.Fatal(err)
			}
			got, err := RuntimeRoot()
			if err != nil || got != want {
				t.Fatalf("got %q, %v; want %q", got, err, want)
			}
			if _, err := os.Stat(got); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("resolver created directory: %v", err)
			}
		})
	}
}
