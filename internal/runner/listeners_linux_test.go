//go:build linux

// Tests runner listeners behavior on Linux.
package runner

import (
	"net"
	"os"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
)

func TestListenerOwnershipAndAddress(t *testing.T) {
	ref, err := procid.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := checkListeners([]procid.Ref{ref}, port); err != nil {
		t.Fatal(err)
	}
	if err := checkListeners(nil, port); err == nil {
		t.Fatal("foreign listener accepted")
	}
	l.Close()
	wildcard, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer wildcard.Close()
	if err := checkListeners([]procid.Ref{ref}, wildcard.Addr().(*net.TCPAddr).Port); err == nil {
		t.Fatal("wildcard listener accepted")
	}
}

func TestUDPConnectedIsNotListener(t *testing.T) {
	ref, err := procid.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("udp4", "192.0.2.1:53")
	if err != nil {
		t.Skipf("no route for UDP client: %v", err)
	}
	defer c.Close()
	if err := checkListeners([]procid.Ref{ref}, 0); err != nil {
		t.Fatal(err)
	}
	u, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	if err := checkListeners([]procid.Ref{ref}, 0); err == nil {
		t.Fatal("wildcard UDP listener accepted")
	}
}
