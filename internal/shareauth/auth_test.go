package shareauth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGate(t *testing.T) {
	credentials, password, err := NewCredentials()
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.Header().Set("Cache-Control", "public, max-age=3600")
		http.SetCookie(w, &http.Cookie{Name: "app", Value: "kept"})
		w.WriteHeader(204)
	})
	handler, err := NewHandler("one", credentials, "https://share.example", next)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://share.example"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/asset.js", "/api/data", "/socket"} {
		w := request("GET", path, "", "", nil)
		if w.Code != 303 || called != 0 {
			t.Fatalf("anonymous %s: %d", path, w.Code)
		}
	}
	if w := request("POST", loginPath, "password="+password, "https://evil.example", nil); w.Code != 403 {
		t.Fatalf("origin %d", w.Code)
	}
	if w := request("POST", loginPath, "password="+strings.Repeat("a", 5000), "https://share.example", nil); w.Code != 400 {
		t.Fatalf("oversize %d", w.Code)
	}
	body := url.Values{"password": {password}, "next": {"//evil.example"}}.Encode()
	w := request("POST", loginPath, body, "https://share.example", nil)
	if w.Code != 303 || w.Header().Get("Location") != "/" {
		t.Fatalf("login %d %q", w.Code, w.Header().Get("Location"))
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing cookie")
	}
	cookie := cookies[0]
	if !strings.HasPrefix(cookie.Name, "__Host-") || !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/" || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge <= 0 {
		t.Fatalf("cookie: %+v", cookie)
	}
	w = request("GET", "/api/data", "", "", cookie)
	if w.Code != 204 || called != 1 || !strings.Contains(w.Header().Get("Set-Cookie"), "app=kept") || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("application response not preserved")
	}
	newer, _, _ := NewCredentials()
	handler, err = NewHandler("one", newer, "https://share.example", next)
	if err != nil {
		t.Fatal(err)
	}
	if w = request("GET", "/api/data", "", "", cookie); w.Code != 303 {
		t.Fatal("old session accepted")
	}
	for i := 0; i < 10; i++ {
		w = request("POST", loginPath, "password=wrong", "https://share.example", nil)
		if w.Code != 401 {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	if w = request("POST", loginPath, "password=wrong", "https://share.example", nil); w.Code != 429 {
		t.Fatalf("throttle: %d", w.Code)
	}
}
func TestSecureRequestAndRedirects(t *testing.T) {
	record, _, _ := NewCredentials()
	h, err := NewHandler("id", record, "https://share.example", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if err != nil {
		t.Fatal(err)
	}
	for _, remote := range []string{"192.0.2.1:1234", "127.0.0.1:1234"} {
		r := httptest.NewRequest("GET", "http://share.example/", nil)
		r.RemoteAddr = remote
		r.Header.Add("X-Forwarded-Proto", "https")
		if strings.HasPrefix(remote, "127.") {
			r.Header.Add("X-Forwarded-Proto", "https")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("untrusted transport %s: %d", remote, w.Code)
		}
	}
	for _, value := range []string{"//evil.test", "/\\evil.test", "/%2f%2fevil.test", "/x\r\nLocation: https://evil.test"} {
		if got := safeRedirect(value); got != "/" {
			t.Fatalf("unsafe redirect %q => %q", value, got)
		}
	}
}

func TestHTTPServerBlocksWebSocketAndForwardsCookies(t *testing.T) {
	record, password, err := NewCredentials()
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler("network", record, "https://share.example", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, e := r.Cookie("app"); e != nil || c.Value != "value" {
			t.Error("application cookie missing")
		}
		if r.URL.Path == "/socket" {
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
			_, _ = rw.Write([]byte{0x81, 1, 'x'})
			_ = rw.Flush()
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, _ := http.NewRequest("GET", server.URL+"/socket", nil)
	req.Host = "share.example"
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("anonymous websocket: %d", resp.StatusCode)
	}
	body := url.Values{"password": {password}}.Encode()
	req, _ = http.NewRequest("POST", server.URL+loginPath, strings.NewReader(body))
	req.Host = "share.example"
	req.Header.Set("Origin", "https://share.example")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cookies := resp.Cookies()
	if resp.StatusCode != 303 || len(cookies) != 1 {
		t.Fatalf("login %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", server.URL+"/api", nil)
	req.Host = "share.example"
	req.AddCookie(cookies[0])
	req.AddCookie(&http.Cookie{Name: "app", Value: "value"})
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("authorized: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", server.URL+"/socket", nil)
	req.Host = "share.example"
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.AddCookie(cookies[0])
	req.AddCookie(&http.Cookie{Name: "app", Value: "value"})
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	frame := make([]byte, 3)
	if _, err := io.ReadFull(resp.Body, frame); err != nil || resp.StatusCode != 101 || string(frame) != string([]byte{0x81, 1, 'x'}) {
		t.Fatalf("authenticated upgrade: status=%d frame=%q err=%v", resp.StatusCode, frame, err)
	}
}
