// Parses Darwin socket_fdinfo using the 64-bit XNU ABI from bsd/sys/proc_info.h.
package runner

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

const darwinSocketInfoSize = 792

func darwinSocketListener(b []byte, wanted int) (bool, error) {
	if len(b) != darwinSocketInfoSize {
		return false, errors.New("short Darwin socket info")
	}
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }
	family, protocol, kind := u32(184), u32(180), u32(256)
	if family != 2 && family != 30 {
		return false, nil
	}
	if protocol != 6 && protocol != 17 {
		return false, nil
	}
	if (protocol == 6 && kind != 2) || (protocol == 17 && kind != 1) {
		return false, errors.New("incomplete Darwin internet socket info")
	}
	if protocol == 6 {
		state := u32(344)
		if state > 11 {
			return false, errors.New("invalid Darwin TCP state")
		}
		if state != 1 {
			return false, nil
		}
	} else if binary.LittleEndian.Uint16(b[192:])&2 != 0 {
		return false, nil
	}
	port := int(binary.BigEndian.Uint16(b[268:270]))
	if port == 0 {
		return false, nil
	}
	vflag := b[288]
	var ip net.IP
	switch family {
	case 2:
		if vflag&1 == 0 {
			return false, errors.New("missing IPv4 socket address")
		}
		ip = net.IP(b[324:328])
	case 30:
		if vflag&2 == 0 {
			return false, errors.New("missing IPv6 socket address")
		}
		ip = net.IP(b[312:328])
	}
	if !ip.IsLoopback() {
		return false, fmt.Errorf("tracked process has non-loopback socket on port %d", port)
	}
	match := protocol == 6 && port == wanted && ip.Equal(net.ParseIP("127.0.0.1"))

	if match && binary.LittleEndian.Uint16(b[188:])&0x0200 != 0 {
		return false, &Error{Code: "OWNERSHIP_UNKNOWN", Err: errors.New("listener uses SO_REUSEPORT; disable port sharing")}
	}
	return match, nil
}
