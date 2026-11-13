//go:build darwin

package runner

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
)

// proc_info and its flavors are defined in xnu proc_info_private.h and
// proc_info.h. Each result is checked for truncation and syscall errors.
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/proc_info.h
func darwinProcInfo(call, pid, flavor, arg int, buf []byte) (int, error) {
	n, _, errno := syscall.Syscall6(syscall.SYS_PROC_INFO, uintptr(call), uintptr(pid), uintptr(flavor), uintptr(arg), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if errno != 0 {
		return 0, errno
	}
	return int(n), nil
}

func darwinDescriptors(pid, flavor int) ([]byte, error) {
	for size := 4096; size <= 16*1024*1024; size *= 2 {
		b := make([]byte, size)
		n, err := darwinProcInfo(2, pid, flavor, 0, b)
		if err != nil {
			return nil, err
		}
		if n < 0 || n > len(b) || n%8 != 0 {
			return nil, errors.New("malformed Darwin descriptor list")
		}
		if n < len(b) {
			return b[:n], nil
		}
	}
	return nil, errors.New("Darwin descriptor list exceeds inspection limit")
}

func checkListeners(refs []procid.Ref, port int) error {
	var err error
	// Normal requests open/close descriptors during inspection. Retry the full
	// inspection rather than accepting a partial snapshot or killing on one race.
	for attempt := 0; attempt < 3; attempt++ {
		if err = checkDarwinListeners(refs, port); err == nil {
			return nil
		}
		if attempt < 2 {
			time.Sleep(5 * time.Millisecond)
		}
	}
	return err
}

func checkDarwinListeners(refs []procid.Ref, port int) error {
	found := port == 0
	for _, ref := range refs {
		ok, err := procid.Check(ref)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		for _, list := range []struct{ flavor, call int }{{1, 3}, {14, 6}} {
			entries, err := darwinDescriptors(ref.PID, list.flavor)
			if err != nil {
				return fmt.Errorf("inspect descriptors of %d: %w", ref.PID, err)
			}
			for off := 0; off < len(entries); off += 8 {
				if binary.LittleEndian.Uint32(entries[off+4:]) != 2 {
					continue
				}
				descriptor := binary.LittleEndian.Uint32(entries[off:])
				buf := make([]byte, darwinSocketInfoSize)
				n, err := darwinProcInfo(list.call, ref.PID, 3, int(descriptor), buf)
				if err != nil {
					return fmt.Errorf("inspect socket %d of %d: %w", descriptor, ref.PID, err)
				}
				if n != len(buf) {
					return errors.New("incomplete Darwin socket inspection")
				}
				// A descriptor can be closed and reused between list snapshots.
				// Re-read its identity and relevant socket state before accepting it.
				again := make([]byte, darwinSocketInfoSize)
				n, err = darwinProcInfo(list.call, ref.PID, 3, int(descriptor), again)
				if err != nil || n != len(again) {
					return fmt.Errorf("cannot recheck socket %d of %d: %v", descriptor, ref.PID, err)
				}
				if !bytes.Equal(buf[160:196], again[160:196]) || !bytes.Equal(buf[256:348], again[256:348]) {
					return errors.New("socket changed during Darwin inspection")
				}
				match, err := darwinSocketListener(buf, port)
				if err != nil {
					return err
				}
				found = found || match
			}
			after, err := darwinDescriptors(ref.PID, list.flavor)
			if err != nil {
				return err
			}
			if !bytes.Equal(entries, after) {
				return errors.New("descriptors changed during Darwin socket inspection")
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
	if !found {
		return fmt.Errorf("no owned loopback listener on port %d", port)
	}
	return nil
}
