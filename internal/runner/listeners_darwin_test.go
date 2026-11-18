// Tests runner listeners behavior on macOS.
package runner

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestDarwinSocketDescriptorSnapshotIgnoresFileChurn(t *testing.T) {
	entry := func(fd, kind uint32) []byte {
		b := make([]byte, 8)
		binary.LittleEndian.PutUint32(b, fd)
		binary.LittleEndian.PutUint32(b[4:], kind)
		return b
	}
	sockets := append(entry(3, 2), entry(9, 2)...)
	first := append(entry(4, 1), sockets...)
	second := append(append(entry(5, 1), sockets...), entry(12, 1)...)
	if !bytes.Equal(darwinSocketDescriptors(first), darwinSocketDescriptors(second)) {
		t.Fatal("non-socket descriptor churn changed socket snapshot")
	}
	added := append(append([]byte(nil), second...), entry(14, 2)...)
	if bytes.Equal(darwinSocketDescriptors(first), darwinSocketDescriptors(added)) {
		t.Fatal("added socket was not detected")
	}
	replaced := append(entry(3, 2), entry(10, 2)...)
	if bytes.Equal(darwinSocketDescriptors(first), darwinSocketDescriptors(replaced)) {
		t.Fatal("replaced socket was not detected")
	}
}
