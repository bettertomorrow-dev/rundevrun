//go:build linux

// Verifies process-owned network listeners on Linux.
package runner

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
)

func checkListeners(refs []procid.Ref, port int) error {
	inodes := map[string]bool{}
	for _, ref := range refs {
		ok, err := procid.Check(ref)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", ref.PID))
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return err
		}
		for _, fd := range fds {
			target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", ref.PID, fd.Name()))
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
				continue
			}
			if err != nil {
				return err
			}
			if strings.HasPrefix(target, "socket:[") && strings.HasSuffix(target, "]") {
				inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = true
			}
		}
		ok, err = procid.Check(ref)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("process changed during listener inspection")
		}
	}
	found := port == 0
	for _, table := range []string{"tcp", "tcp6", "udp", "udp6"} {
		f, err := os.Open(filepath.Join("/proc/net", table))
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(f)
		scanner.Scan()
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 10 {
				f.Close()
				return errors.New("malformed proc socket table")
			}
			tcp := strings.HasPrefix(table, "tcp")
			if (tcp && fields[3] != "0A") || (!tcp && fields[3] != "07") {
				continue
			}

			if !inodes[fields[9]] && (!tcp || port == 0) {
				continue
			}
			parts := strings.Split(fields[1], ":")
			if len(parts) != 2 {
				f.Close()
				return errors.New("malformed socket address")
			}
			ip, err := procIP(parts[0])
			if err != nil {
				f.Close()
				return err
			}
			p, err := strconv.ParseUint(parts[1], 16, 16)
			if err != nil {
				f.Close()
				return err
			}
			if !inodes[fields[9]] {
				if int(p) == port && (ip.Equal(net.ParseIP("127.0.0.1")) || ip.IsUnspecified()) {
					f.Close()
					return &Error{Code: "OWNERSHIP_UNKNOWN", Err: errors.New("listener endpoint is shared with an untracked process; disable port sharing")}
				}
				continue
			}
			if !ip.IsLoopback() {
				f.Close()
				return fmt.Errorf("tracked process has non-loopback %s socket on port %d", table, p)
			}
			if strings.HasPrefix(table, "tcp") && int(p) == port && ip.Equal(net.ParseIP("127.0.0.1")) {
				found = true
			}
		}
		err = scanner.Err()
		if e := f.Close(); err == nil {
			err = e
		}
		if err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("no owned loopback listener on port %d", port)
	}
	return nil
}

func procIP(s string) (net.IP, error) {
	b, err := hex.DecodeString(s)
	if err != nil || (len(b) != 4 && len(b) != 16) {
		return nil, errors.New("invalid proc socket address")
	}
	for i := 0; i < len(b); i += 4 {
		b[i], b[i+3] = b[i+3], b[i]
		b[i+1], b[i+2] = b[i+2], b[i+1]
	}
	return net.IP(b), nil
}
