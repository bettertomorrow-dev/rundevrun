// Resolves the runtime state directory.
package state

import (
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
