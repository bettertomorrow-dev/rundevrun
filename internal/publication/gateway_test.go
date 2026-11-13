package publication

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bettertomorrow-dev/rundevrun/internal/shareauth"
)

func TestGatewayBodyDeadline(t *testing.T) {
	record, _, err := shareauth.NewCredentials()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := shareauth.NewHandler("deadline", record, "https://share.example", http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"https://share.example", "https://rejected.example"} {
		t.Run(origin, func(t *testing.T) {
			server := httptest.NewUnstartedServer(handler)
			server.Config = newGatewayServer(handler)
			if server.Config.ReadTimeout <= 0 || server.Config.WriteTimeout != 0 {
				t.Fatal("unsafe gateway deadlines")
			}
			server.Config.ReadTimeout = 50 * time.Millisecond
			server.Start()
			defer server.Close()
			conn, err := net.Dial("tcp", server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			_, err = fmt.Fprintf(conn, "POST /_rdr/login HTTP/1.1\r\nHost: share.example\r\nX-Forwarded-Proto: https\r\nOrigin: %s\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 32\r\n\r\npassword=", origin)
			if err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != 400 && response.StatusCode != 403 {
				t.Fatalf("status %d", response.StatusCode)
			}
		})
	}
}

func TestGatewayDeadlinePreservesStreamingAndHijack(t *testing.T) {
	for _, path := range []string{"/stream", "/upgrade"} {
		t.Run(path, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/upgrade" {
					conn, rw, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					time.Sleep(100 * time.Millisecond)
					_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nlate")
					_ = rw.Flush()
					return
				}
				_, _ = io.WriteString(w, "first")
				w.(http.Flusher).Flush()
				time.Sleep(100 * time.Millisecond)
				_, _ = io.WriteString(w, "late")
			})
			server := httptest.NewUnstartedServer(handler)
			server.Config = newGatewayServer(handler)
			server.Config.ReadTimeout = 20 * time.Millisecond
			server.Start()
			defer server.Close()
			client := &http.Client{Timeout: time.Second}
			response, err := client.Get(server.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || !strings.HasSuffix(string(body), "late") {
				t.Fatalf("late response %q: %v", body, err)
			}
		})
	}
}
