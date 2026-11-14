// Package shareauth protects a publication with a generated password.
package shareauth

import (
	"bufio"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const iterations = 100000
const loginPath = "/_rdr/login"
const sessionTTL = 12 * time.Hour

type Record struct {
	Salt       []byte `json:"salt"`
	Verifier   []byte `json:"verifier"`
	SigningKey []byte `json:"signing_key"`
}

func NewCredentials() (Record, string, error) {
	var r Record
	password := make([]byte, 24)
	r.Salt = make([]byte, 16)
	r.SigningKey = make([]byte, 32)
	for _, b := range [][]byte{password, r.Salt, r.SigningKey} {
		if _, err := rand.Read(b); err != nil {
			return Record{}, "", err
		}
	}
	plain := base64.RawURLEncoding.EncodeToString(password)
	verifier, err := pbkdf2.Key(sha256.New, plain, r.Salt, iterations, 32)
	if err != nil {
		return Record{}, "", err
	}
	r.Verifier = verifier
	return r, plain, nil
}

type gate struct {
	origin, authority, name string
	credentials             Record
	next                    http.Handler
	mu                      sync.Mutex
	window                  time.Time
	attempts                int
}

var form = template.Must(template.New("login").Parse(`<!doctype html><html><head><meta charset="utf-8"><title>Sign in</title></head><body><form method="post" action="/_rdr/login"><label>Password <input type="password" name="password" required autofocus></label><input type="hidden" name="next" value="{{.}}"><button type="submit">Sign in</button></form></body></html>`))

func NewHandler(publicationID string, credentials Record, publicURL string, next http.Handler) (http.Handler, error) {
	u, err := url.Parse(publicURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" || next == nil || publicationID == "" || len(credentials.Salt) < 16 || len(credentials.Verifier) != 32 || len(credentials.SigningKey) < 32 {
		return nil, errors.New("invalid publication authentication configuration")
	}
	if u.Hostname() == "" || strings.ContainsAny(u.Host, "\\\r\n ") {
		return nil, errors.New("invalid publication URL")
	}
	digest := sha256.Sum256([]byte(publicationID))
	r := Record{Salt: append([]byte(nil), credentials.Salt...), Verifier: append([]byte(nil), credentials.Verifier...), SigningKey: append([]byte(nil), credentials.SigningKey...)}
	return &gate{origin: "https://" + u.Host, authority: u.Host, name: "__Host-rdr-" + hex.EncodeToString(digest[:12]), credentials: r, next: next}, nil
}
func safeRedirect(value string) string {
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.ContainsAny(value, "\\\r\n") {
		return "/"
	}
	u, e := url.Parse(value)
	if e != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") || strings.ContainsAny(u.Path, "\\\r\n") {
		return "/"
	}
	return value
}
func (g *gate) secureRequest(r *http.Request) bool {
	if !strings.EqualFold(r.Host, g.authority) {
		return false
	}
	if r.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback() && len(r.Header.Values("X-Forwarded-Proto")) == 1 && r.Header.Get("X-Forwarded-Proto") == "https"
}
func (g *gate) signature(exp string) string {
	mac := hmac.New(sha256.New, g.credentials.SigningKey)
	mac.Write([]byte(exp))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (g *gate) authenticated(r *http.Request) bool {
	c, err := r.Cookie(g.name)
	if err != nil {
		return false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 2 || !hmac.Equal([]byte(parts[1]), []byte(g.signature(parts[0]))) {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	return err == nil && exp > time.Now().Unix() && exp <= time.Now().Add(sessionTTL).Unix()
}
func (g *gate) allowAttempt() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if g.window.IsZero() || now.Sub(g.window) >= time.Minute {
		g.window = now
		g.attempts = 0
	}
	if g.attempts >= 10 {
		return false
	}
	g.attempts++
	return true
}
func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !g.secureRequest(r) {
		http.Error(w, "invalid publication request", http.StatusBadRequest)
		return
	}
	if r.URL.Path == loginPath {
		w.Header().Set("Cache-Control", "no-store")
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = form.Execute(w, safeRedirect(r.URL.Query().Get("next")))
		case http.MethodPost:
			if len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != g.origin {
				http.Error(w, "invalid origin", http.StatusForbidden)
				return
			}
			if !g.allowAttempt() {
				http.Error(w, "too many attempts", http.StatusTooManyRequests)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 4096)
			if err := r.ParseForm(); err != nil {
				http.Error(w, "invalid form", http.StatusBadRequest)
				return
			}
			password := r.PostForm.Get("password")
			verifier, err := pbkdf2.Key(sha256.New, password, g.credentials.Salt, iterations, 32)
			if err != nil || subtle.ConstantTimeCompare(verifier, g.credentials.Verifier) != 1 {
				http.Error(w, "invalid password", http.StatusUnauthorized)
				return
			}
			exp := strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
			http.SetCookie(w, &http.Cookie{Name: g.name, Value: exp + "." + g.signature(exp), Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds())})
			http.Redirect(w, r, safeRedirect(r.PostForm.Get("next")), http.StatusSeeOther)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if strings.HasPrefix(r.URL.Path, loginPath+"/") {
		http.NotFound(w, r)
		return
	}
	if !g.authenticated(r) {
		w.Header().Set("Cache-Control", "no-store")
		target := safeRedirect(r.URL.RequestURI())
		http.Redirect(w, r, loginPath+"?next="+url.QueryEscape(target), http.StatusSeeOther)
		return
	}
	g.next.ServeHTTP(privateResponse{w}, r)
	// A handler may return without writing, leaving the server to send its implicit 200.
	w.Header().Set("Cache-Control", "private, no-store")
}

// Authenticated responses must not be reused by a shared cache for another visitor.
// Apply the header when it is written so an upstream's public cache policy cannot replace it.
type privateResponse struct{ http.ResponseWriter }

func (w privateResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w privateResponse) WriteHeader(status int) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.ResponseWriter.WriteHeader(status)
}
func (w privateResponse) Write(b []byte) (int, error) {
	w.Header().Set("Cache-Control", "private, no-store")
	return w.ResponseWriter.Write(b)
}
func (w privateResponse) Flush() {
	w.Header().Set("Cache-Control", "private, no-store")
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w privateResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}
