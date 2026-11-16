// Reads process and boot identities on Linux.
package procid

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
)

func Identity(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {

		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			return "", ErrGone
		}
		return "", err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return "", fmt.Errorf("malformed process stat for %d", pid)
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return "", fmt.Errorf("short process stat for %d", pid)
	}
	if f[0] == "Z" || f[0] == "X" {
		return "", ErrGone
	}
	return f[19], nil
}

func bootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("empty boot identity")
	}
	return s, nil
}
