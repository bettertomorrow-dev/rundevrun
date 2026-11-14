//go:build linux

package runner

import (
	"bufio"
	"context"
	"fmt"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/bettertomorrow-dev/rundevrun/internal/procid"
)

func TestForeignLoopbackHelper(t *testing.T) {
	port := os.Getenv("RDR_FOREIGN_LISTENER_PORT")
	if port == "" {
		return
	}
	var ln net.Listener
	var err error
	if os.Getenv("RDR_FOREIGN_REUSE") == "yes" {
		ln, err = reuseListener(net.JoinHostPort("127.0.0.1", port))
	} else {
		ln, err = net.Listen("tcp4", net.JoinHostPort("127.0.0.1", port))
	}
	if err != nil {
		os.Exit(2)
	}
	fmt.Println("ready")
	defer ln.Close()
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(0)
}
func TestListenerMustOwnDialAddress(t *testing.T) {
	own, err := net.Listen("tcp4", "127.0.0.2:0")
	if err != nil {
		t.Skipf("127.0.0.2 unavailable: %v", err)
	}
	defer own.Close()
	port := own.Addr().(*net.TCPAddr).Port
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe, "-test.run=^TestForeignLoopbackHelper$")
	child.Env = append(os.Environ(), "RDR_FOREIGN_LISTENER_PORT="+strconv.Itoa(port))
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = child.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("foreign fixture: %q %v", line, err)
	}
	self, err := procid.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err = checkListeners([]procid.Ref{self}, port); err == nil {
		t.Fatal("accepted listener at a different loopback address")
	}
	conn, err := net.Dial("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("foreign fixture was disrupted: %v", err)
	}
	_ = conn.Close()
}
func TestIPv6OnlyDoesNotOwnIPv4DialAddress(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 unavailable: %v", err)
	}
	defer ln.Close()
	self, err := procid.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err = checkListeners([]procid.Ref{self}, ln.Addr().(*net.TCPAddr).Port); err == nil {
		t.Fatal("accepted IPv6-only listener for IPv4 dial")
	}
}

func reuseListener(address string) (net.Listener, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var socketErr error
		if err := c.Control(func(fd uintptr) { socketErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1) }); err != nil {
			return err
		}
		return socketErr
	}}
	return lc.Listen(context.Background(), "tcp4", address)
}
func TestSharedReusePortRefused(t *testing.T) {
	own, err := reuseListener("127.0.0.1:0")
	if err != nil {
		t.Skipf("SO_REUSEPORT unavailable: %v", err)
	}
	defer own.Close()
	port := own.Addr().(*net.TCPAddr).Port
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe, "-test.run=^TestForeignLoopbackHelper$")
	child.Env = append(os.Environ(), "RDR_FOREIGN_LISTENER_PORT="+strconv.Itoa(port), "RDR_FOREIGN_REUSE=yes")
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = child.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("foreign fixture: %q %v", line, err)
	}
	self, err := procid.Observe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err = checkListeners([]procid.Ref{self}, port); err == nil {
		t.Fatal("accepted shared listener")
	}
	if err = child.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("foreign fixture disrupted: %v", err)
	}
}
