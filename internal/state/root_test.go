// Tests runtime directory selection and protection of legacy installations.
package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestCheckLegacyRoot(t *testing.T) {
	for _, xdg := range []bool{false, true} {
		t.Run(map[bool]string{false: "home", true: "xdg"}[xdg], func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("HOME", base)
			t.Setenv("RDR_HOME", "")
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
			t.Setenv("XDG_STATE_HOME", "")
			legacy := filepath.Join(base, ".local", "state", "rundevrun")
			if xdg {
				t.Setenv("XDG_STATE_HOME", filepath.Join(base, "old"))
				legacy = filepath.Join(base, "old", "rundevrun")
			}
			root, err := RuntimeRoot()
			if err != nil {
				t.Fatal(err)
			}
			if err := CheckLegacyRoot(root); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(legacy, 0700); err != nil {
				t.Fatal(err)
			}
			if err := CheckLegacyRoot(root); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(legacy, "registry.json"), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := CheckLegacyRoot(root); err == nil || !strings.Contains(err.Error(), legacy) || !strings.Contains(err.Error(), "RDR_HOME=") {
				t.Fatalf("legacy state not protected: %v", err)
			}
			t.Setenv("RDR_HOME", legacy)
			if err := CheckLegacyRoot(legacy); err != nil {
				t.Fatal(err)
			}
		})
	}
}
