// Resolves runtime storage and detects installations using the former default.
package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func RuntimeRoot() (string, error) {
	if root := os.Getenv("RDR_HOME"); root != "" {
		return filepath.Abs(root)
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Abs(filepath.Join(base, "rundevrun", "state"))
}

func CheckLegacyRoot(root string) error {
	if os.Getenv("RDR_HOME") != "" {
		return nil
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		base = filepath.Join(home, ".local", "state")
	}
	legacy, err := filepath.Abs(filepath.Join(base, "rundevrun"))
	if err != nil || legacy == root {
		return err
	}
	entries, err := os.ReadDir(legacy)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("legacy runtime state exists at %s; set RDR_HOME=%q to keep using it; stop services and the proxy before resetting it to use %s", legacy, legacy, root)
	}
	return nil
}
