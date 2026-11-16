// Allocates loopback ports for publication tests on Linux.
package publication

import (
	"fmt"
	"net"
	"regexp"
	"testing"
)

var automaticTestPort = regexp.MustCompile(`(?m)^[ \t]*port[ \t]*=[ \t]*["']auto["']`)

func isolatedHTTPPorts(t *testing.T, config string) string {
	t.Helper()
	var held []net.Listener
	defer func() {
		for _, listener := range held {
			_ = listener.Close()
		}
	}()
	return automaticTestPort.ReplaceAllStringFunc(config, func(string) string {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, listener)
		return fmt.Sprintf("port = %d", listener.Addr().(*net.TCPAddr).Port)
	})
}
