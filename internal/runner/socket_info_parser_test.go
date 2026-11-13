package runner

import (
	"encoding/binary"
	"testing"
)

func TestDarwinSocketInfo(t *testing.T) {
	makeSocket := func(family, proto, kind uint32) []byte {
		b := make([]byte, darwinSocketInfoSize)
		binary.LittleEndian.PutUint32(b[184:], family)
		binary.LittleEndian.PutUint32(b[180:], proto)
		binary.LittleEndian.PutUint32(b[256:], kind)
		binary.BigEndian.PutUint16(b[268:], 8080)
		if family == 2 {
			b[288] = 1
			copy(b[324:], []byte{127, 0, 0, 1})
		} else {
			b[288] = 2
			b[327] = 1
		}
		if proto == 6 {
			binary.LittleEndian.PutUint32(b[344:], 1)
		}
		return b
	}
	tests := []struct {
		name           string
		b              []byte
		found, failure bool
	}{
		{"tcp IPv4", makeSocket(2, 6, 2), true, false},
		{"shared port", func() []byte { b := makeSocket(2, 6, 2); binary.LittleEndian.PutUint16(b[188:], 0x200); return b }(), false, true},
		{"other IPv4", func() []byte { b := makeSocket(2, 6, 2); b[327] = 2; return b }(), false, false},
		{"tcp IPv6", makeSocket(30, 6, 2), false, false},
		{"IPv4 mapped IPv6", func() []byte {
			b := makeSocket(30, 6, 2)
			copy(b[312:328], []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 255, 255, 127, 0, 0, 1})
			return b
		}(), true, false},
		{"wildcard IPv4", func() []byte { b := makeSocket(2, 6, 2); clear(b[324:328]); return b }(), false, true},
		{"wildcard IPv6", func() []byte { b := makeSocket(30, 6, 2); clear(b[312:328]); return b }(), false, true},
		{"connected tcp", func() []byte {
			b := makeSocket(2, 6, 2)
			binary.LittleEndian.PutUint32(b[344:], 4)
			clear(b[324:328])
			return b
		}(), false, false},
		{"connected udp", func() []byte { b := makeSocket(2, 17, 1); b[192] = 2; clear(b[324:328]); return b }(), false, false},
		{"unconnected udp", func() []byte { b := makeSocket(2, 17, 1); clear(b[324:328]); return b }(), false, true},
		{"wrong kind", makeSocket(2, 6, 1), false, true},
		{"short", []byte{1}, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			found, err := darwinSocketListener(tc.b, 8080)
			if found != tc.found || (err != nil) != tc.failure {
				t.Fatalf("found=%v err=%v", found, err)
			}
		})
	}
}
