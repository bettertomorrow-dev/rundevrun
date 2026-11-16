// Tests networking dns behavior.
package networking

import (
	"context"
	"encoding/binary"
	"net"
	"slices"
	"sync/atomic"
	"testing"
)

func txtServer(t *testing.T, records *atomic.Value) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		packet := make([]byte, 2048)
		for {
			n, addr, err := conn.ReadFrom(packet)
			if err != nil {
				return
			}
			end := 12
			for end < n && packet[end] != 0 {
				end += int(packet[end]) + 1
			}
			end += 5
			if end > n {
				continue
			}
			answer := append([]byte(nil), packet[:end]...)
			values := records.Load().([]string)
			binary.BigEndian.PutUint16(answer[2:4], 0x8500)
			binary.BigEndian.PutUint16(answer[6:8], uint16(len(values)))
			binary.BigEndian.PutUint16(answer[8:10], 0)
			binary.BigEndian.PutUint16(answer[10:12], 0)
			for _, value := range values {
				answer = append(answer, 0xc0, 0x0c, 0, 16, 0, 1, 0, 0, 0, 60, 0, byte(len(value)+1), byte(len(value)))
				answer = append(answer, value...)
			}
			_, _ = conn.WriteTo(answer, addr)
		}
	}()
	t.Cleanup(func() { conn.Close(); <-done })
	return conn.LocalAddr().String()
}

func TestAuthoritativeTXTRequiresPropagationToEveryServer(t *testing.T) {
	var first, second atomic.Value
	first.Store([]string{"proof", "unrelated"})
	second.Store([]string{"unrelated"})
	servers := []string{txtServer(t, &first), txtServer(t, &second)}

	name := "rdr-acme-fixture.invalid"
	values, err := lookupAuthoritativeTXT(context.Background(), name, servers)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(values, "proof") {
		t.Fatal("challenge accepted before every authoritative server has it")
	}
	second.Store([]string{"proof", "unrelated"})
	values, err = lookupAuthoritativeTXT(context.Background(), name, servers)
	if err != nil || !slices.Contains(values, "proof") || !slices.Contains(values, "unrelated") {
		t.Fatalf("direct authoritative lookup failed: %v, %v", values, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lookupAuthoritativeTXT(ctx, name, servers); err == nil {
		t.Fatal("canceled lookup succeeded")
	}
}
