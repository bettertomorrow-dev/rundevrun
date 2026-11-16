// Reads process and boot identities on macOS.
package procid

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

const szomb = 5

func Identity(pid int) (string, error) {

	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) {
			return "", ErrGone
		}
		return "", err
	}
	if len(processes) == 0 {
		return "", ErrGone
	}
	if len(processes) != 1 {
		return "", fmt.Errorf("unexpected process count for pid %d: %d", pid, len(processes))
	}
	kp := processes[0]
	if kp.Proc.P_pid != int32(pid) {
		return "", ErrGone
	}
	if kp.Proc.P_stat == szomb {
		return "", ErrGone
	}
	st := kp.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", st.Sec, st.Usec), nil
}

func bootID() (string, error) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d.%06d", tv.Sec, tv.Usec), nil
}
