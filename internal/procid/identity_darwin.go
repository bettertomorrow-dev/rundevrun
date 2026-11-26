// Reads process and boot identities on macOS.
package procid

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

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
	id, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", errors.New("empty boot session UUID")
	}
	return id, nil
}

func legacyBootTime(id string) (time.Time, bool) {
	seconds, micros, ok := strings.Cut(id, ".")
	if !ok || len(micros) != 6 {
		return time.Time{}, false
	}
	sec, secErr := strconv.ParseInt(seconds, 10, 64)
	usec, usecErr := strconv.ParseUint(micros, 10, 32)
	if secErr != nil || usecErr != nil || sec <= 0 || usec >= 1000000 || fmt.Sprintf("%d.%06d", sec, usec) != id {
		return time.Time{}, false
	}
	return time.Unix(sec, int64(usec)*1000), true
}

func LegacyBootID(id string) bool {
	_, ok := legacyBootTime(id)
	return ok
}

func matchesBootID(recorded, current string) (bool, error) {
	if recorded == current {
		return true, nil
	}
	legacy, ok := legacyBootTime(recorded)
	if !ok {
		return false, nil
	}
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return false, err
	}
	offset := legacy.Sub(time.Unix(tv.Sec, int64(tv.Usec)*1000))
	return offset >= -time.Minute && offset <= time.Minute, nil
}
