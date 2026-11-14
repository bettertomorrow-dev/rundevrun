package procid

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

const szomb = 5 // kinfo_proc p_stat for a zombie

// Identity is the process start time in microseconds, from the kernel's proc
// table. It changes whenever a PID is recycled.
func Identity(pid int) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) {
			return "", ErrGone
		}
		return "", err
	}
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
