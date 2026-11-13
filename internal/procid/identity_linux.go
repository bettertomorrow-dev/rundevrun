package procid

import (
	"fmt"
	"os"
	"strings"
)

// Identity is field 22 of /proc/<pid>/stat (start time in clock ticks since
// boot). It changes whenever a PID is recycled.
func Identity(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrGone
		}
		return "", err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')') // the command name may contain spaces and parentheses
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
