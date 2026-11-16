// Package routing holds the local proxy configuration and TLS credentials.
package routing

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

const defaultHostname = "{service}-{worktree}-{project}.{domain}"

type Config struct {
	Version int      `toml:"version"`
	Routing Settings `toml:"routing"`
}
type Settings struct {
	Enabled           bool        `toml:"enabled"`
	Domain            string      `toml:"domain"`
	Hostname          string      `toml:"hostname"`
	HTTPListen        string      `toml:"http_listen"`
	HTTPSListen       string      `toml:"https_listen"`
	ExternalHTTPSPort *int        `toml:"external_https_port"`
	TrustedProxies    []string    `toml:"trusted_proxies"`
	TLS               TLSSettings `toml:"tls"`
}
type TLSSettings struct {
	Mode         string        `toml:"mode"`
	Certificates []Certificate `toml:"certificates"`
}
type Certificate struct {
	CertFile string `toml:"cert_file"`
	KeyFile  string `toml:"key_file"`
}

type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string { return e.Code + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }
func invalid(err error) error  { return &Error{Code: "CONFIG_INVALID", Err: err} }

func DefaultPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "rundevrun", "config.toml"), nil
}

func Load(path string) (Config, error) {
	c := Config{Version: 1, Routing: Settings{Hostname: defaultHostname, HTTPListen: "127.0.0.1:8080", HTTPSListen: "127.0.0.1:8443", TrustedProxies: []string{}}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return Config{}, err
	}

	var raw map[string]any
	if err = toml.Unmarshal(data, &raw); err != nil {
		return Config{}, invalid(err)
	}
	if _, ok := raw["version"]; !ok {
		return Config{}, invalid(errors.New("version is required"))
	}
	d := toml.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return Config{}, invalid(err)
	}
	if c.Version != 1 {
		return Config{}, &Error{Code: "VERSION_UNSUPPORTED", Err: errors.New("version must be 1")}
	}
	for _, address := range []string{c.Routing.HTTPListen, c.Routing.HTTPSListen} {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return Config{}, invalid(errors.New("listener must be a loopback IP and port"))
		}
		ip := net.ParseIP(host)
		n, e := strconv.Atoi(port)
		if ip == nil || !ip.IsLoopback() || e != nil || n < 1 || n > 65535 {
			return Config{}, invalid(errors.New("listener must be a loopback IP and port"))
		}
	}
	if p := c.Routing.ExternalHTTPSPort; p != nil && (*p < 1 || *p > 65535) {
		return Config{}, invalid(errors.New("invalid external_https_port"))
	}
	for _, entry := range c.Routing.TrustedProxies {
		ip := net.ParseIP(entry)
		if ip != nil && ip.IsLoopback() {
			continue
		}
		ip, network, e := net.ParseCIDR(entry)
		if e != nil || !ip.IsLoopback() || !network.IP.IsLoopback() {
			return Config{}, invalid(errors.New("trusted_proxies must contain loopback IPs or CIDRs"))
		}
		ones, bits := network.Mask.Size()
		if (bits == 32 && ones < 8) || (bits == 128 && ones != 128) {
			return Config{}, invalid(errors.New("trusted proxy network must be loopback only"))
		}
	}
	if c.Routing.Domain != "" {
		if _, err = validateDNS(strings.ToLower(c.Routing.Domain)); err != nil {
			return Config{}, invalid(fmt.Errorf("domain: %w", err))
		}
		c.Routing.Domain = strings.ToLower(c.Routing.Domain)
	}
	if c.Routing.Enabled && c.Routing.Domain == "" {
		return Config{}, invalid(errors.New("routing.domain is required when enabled"))
	}
	if _, err = c.Hostname("service", "worktree", "project", "example.test"); err != nil {
		return Config{}, invalid(fmt.Errorf("hostname template: %w", err))
	}
	if err := c.validateTLSMode(); err != nil {
		return Config{}, err
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Config{}, err
	}
	for i := range c.Routing.TLS.Certificates {
		cert := &c.Routing.TLS.Certificates[i]
		if cert.CertFile == "" || (!c.ExternalTLS() && cert.KeyFile == "") {
			return Config{}, invalid(errors.New("certificate requires cert_file and, in provided mode, key_file"))
		}
		if !filepath.IsAbs(cert.CertFile) {
			cert.CertFile = filepath.Join(base, cert.CertFile)
		}
		if cert.KeyFile != "" && !filepath.IsAbs(cert.KeyFile) {
			cert.KeyFile = filepath.Join(base, cert.KeyFile)
		}
	}
	return c, nil
}

func (c Config) URL(host string) (string, error) {
	host, err := validateDNS(strings.ToLower(host))
	if err != nil {
		return "", err
	}
	_, port, err := net.SplitHostPort(c.Routing.HTTPSListen)
	if err != nil {
		return "", err
	}
	if c.Routing.ExternalHTTPSPort != nil {
		port = strconv.Itoa(*c.Routing.ExternalHTTPSPort)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("invalid HTTPS port")
	}
	if port == "443" {
		return "https://" + host, nil
	}
	return "https://" + net.JoinHostPort(host, port), nil
}

func (c Config) ExternalTLS() bool { return c.Routing.TLS.Mode == "external" }
func (c Config) validateTLSMode() error {
	mode := c.Routing.TLS.Mode
	if mode != "" && mode != "provided" && mode != "external" {
		return invalid(errors.New("routing.tls.mode must be provided or external"))
	}
	if mode == "external" {
		if len(c.Routing.TrustedProxies) == 0 || c.Routing.ExternalHTTPSPort == nil {
			return invalid(errors.New("external TLS requires trusted_proxies and explicit external_https_port"))
		}
		if *c.Routing.ExternalHTTPSPort < 1 || *c.Routing.ExternalHTTPSPort > 65535 {
			return invalid(errors.New("invalid external_https_port"))
		}
		for _, entry := range c.Routing.TrustedProxies {
			ip := net.ParseIP(entry)
			if ip != nil && ip.IsLoopback() {
				continue
			}
			ip, network, err := net.ParseCIDR(entry)
			if err != nil || !ip.IsLoopback() || !network.IP.IsLoopback() {
				return invalid(errors.New("trusted_proxies must contain loopback IPs or CIDRs"))
			}
			ones, bits := network.Mask.Size()
			if (bits == 32 && ones < 8) || (bits == 128 && ones != 128) {
				return invalid(errors.New("trusted proxy network must be loopback only"))
			}
		}
		if len(c.Routing.TLS.Certificates) == 0 {
			return invalid(errors.New("external TLS requires a public cert_file"))
		}
		for _, cert := range c.Routing.TLS.Certificates {
			if cert.CertFile == "" || cert.KeyFile != "" {
				return invalid(errors.New("external TLS requires cert_file and forbids key_file"))
			}
		}
	}
	return nil
}
