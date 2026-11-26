// Loads and validates project configuration.
package config

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

type Config struct {
	Version  int                `toml:"version"`
	Project  Project            `toml:"project"`
	Services map[string]Service `toml:"services"`
	Routing  *Routing           `toml:"routing"`
	Filename string             `toml:"-"`
	Root     string             `toml:"-"`
	Digest   string             `toml:"-"`
}
type Project struct {
	Name     string   `toml:"name"`
	EnvFiles []string `toml:"env_files"`
}
type Routing struct {
	Domain string `toml:"domain"`
}
type Service struct {
	Restart            string            `toml:"restart"`
	RestartDelay       string            `toml:"restart_delay"`
	RestartMaxDelay    string            `toml:"restart_max_delay"`
	RestartStableAfter string            `toml:"restart_stable_after"`
	RestartMaxAttempts int               `toml:"restart_max_attempts"`
	RestartConsumers   bool              `toml:"restart_consumers"`
	Command            string            `toml:"command"`
	CWD                string            `toml:"cwd"`
	Build              string            `toml:"build"`
	Protocol           string            `toml:"protocol"`
	Port               any               `toml:"port"`
	Singleton          bool              `toml:"singleton"`
	DependsOn          []string          `toml:"depends_on"`
	Env                map[string]string `toml:"env"`
	BuildTimeout       string            `toml:"build_timeout"`
	StartTimeout       string            `toml:"start_timeout"`
	StopTimeout        string            `toml:"stop_timeout"`
	HookTimeout        string            `toml:"hook_timeout"`
	Health             map[string]any    `toml:"health"`
	Hooks              map[string]string `toml:"hooks"`
	Routes             []Route           `toml:"routes"`
	Share              map[string]any    `toml:"share"`
}
type Route struct {
	Path        string `toml:"path"`
	Dependency  string `toml:"dependency"`
	StripPrefix bool   `toml:"strip_prefix"`
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
var envRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func Load(filename string) (*Config, error) {
	return LoadForRoot(filename, filepath.Dir(filename))
}

func LoadForRoot(filename, root string) (*Config, error) {
	b, e := os.ReadFile(filename)
	if e != nil {
		return nil, e
	}
	var c Config
	var raw map[string]any
	if e := toml.Unmarshal(b, &raw); e != nil {
		return nil, configFailure(filename, "toml", e)
	}
	d := toml.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil {
		return nil, configFailure(filename, "toml", e)
	}
	c.Filename = filename
	c.Root = root
	if e := validateEmptyFields(raw); e != nil {
		field := strings.SplitN(e.Error(), ":", 2)[0]
		return nil, configFailure(filename, field, e)
	}
	h := sha256.Sum256(b)
	c.Digest = fmt.Sprintf("%x", h)
	if e = c.Validate(); e != nil {
		code := "CONFIG_INVALID"
		if errors.Is(e, ErrVersionUnsupported) {
			code = "VERSION_UNSUPPORTED"
		}
		field := strings.SplitN(e.Error(), ":", 2)[0]
		return nil, fmt.Errorf("%s: %w", filename, &Error{Code: code, Field: field, Err: e})
	}
	return &c, nil
}
func configFailure(filename, field string, err error) error {
	return fmt.Errorf("%s: %w", filename, &Error{Code: "CONFIG_INVALID", Field: field, Err: err})
}

func (c *Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("version: %w: must be 1", ErrVersionUnsupported)
	}
	if !nameRE.MatchString(c.Project.Name) {
		return fmt.Errorf("project.name: invalid name")
	}
	for _, path := range c.Project.EnvFiles {
		if !filepath.IsLocal(path) || filepath.Clean(path) == "." {
			return fmt.Errorf("project.env_files: paths must name files relative to the checkout root")
		}
	}
	if len(c.Services) == 0 {
		return fmt.Errorf("services: must not be empty")
	}
	base := c.Root
	if base == "" {
		base = filepath.Dir(c.Filename)
	}
	root, e := filepath.EvalSymlinks(base)
	if e != nil {
		return fmt.Errorf("worktree directory: %w", e)
	}
	var names []string
	for n := range c.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s := c.Services[n]
		if s.CWD == "" {
			s.CWD = "."
		}
		if s.BuildTimeout == "" {
			s.BuildTimeout = "5m"
		}
		if s.StartTimeout == "" {
			s.StartTimeout = "30s"
		}
		if s.StopTimeout == "" {
			s.StopTimeout = "20s"
		}
		if s.HookTimeout == "" {
			s.HookTimeout = "60s"
		}
		if s.Restart == "" {
			s.Restart = "never"
		}
		if s.RestartDelay == "" {
			s.RestartDelay = "1s"
		}
		if s.RestartMaxDelay == "" {
			s.RestartMaxDelay = "30s"
		}
		if s.RestartStableAfter == "" {
			s.RestartStableAfter = "60s"
		}
		if s.RestartMaxAttempts == 0 {
			s.RestartMaxAttempts = 10
		}
		if s.Protocol == "" {
			if s.Port == nil {
				s.Protocol = "process"
			} else {
				s.Protocol = "http"
			}
		}
		c.Services[n] = s
	}
	for _, n := range names {
		s := c.Services[n]
		p := "services." + n
		if !nameRE.MatchString(n) {
			return fmt.Errorf("%s: invalid name", p)
		}
		if strings.TrimSpace(s.Command) == "" {
			return fmt.Errorf("%s.command: required", p)
		}
		if s.CWD == "" {
			s.CWD = "."
		}
		if s.BuildTimeout == "" {
			s.BuildTimeout = "5m"
		}
		if s.StartTimeout == "" {
			s.StartTimeout = "30s"
		}
		if s.StopTimeout == "" {
			s.StopTimeout = "20s"
		}
		if s.HookTimeout == "" {
			s.HookTimeout = "60s"
		}
		if !filepath.IsLocal(s.CWD) {
			return fmt.Errorf("%s.cwd: must be relative", p)
		}
		cwd, e := filepath.EvalSymlinks(filepath.Join(root, s.CWD))
		if e != nil {
			return fmt.Errorf("%s.cwd: %w", p, e)
		}
		if !within(root, cwd) {
			return fmt.Errorf("%s.cwd: escapes worktree", p)
		}
		st, er := os.Stat(cwd)
		if er != nil {
			return fmt.Errorf("%s.cwd: %w", p, er)
		}
		if !st.IsDir() {
			return fmt.Errorf("%s.cwd: must be a directory", p)
		}
		if s.Build != "" {
			if e := validateCommandFor(s.Build, c, n); e != nil {
				return fmt.Errorf("%s.build: %w", p, e)
			}
		}
		if e := validateCommandFor(s.Command, c, n); e != nil {
			return fmt.Errorf("%s.command: %w", p, e)
		}
		if s.Protocol != "process" && s.Protocol != "tcp" && s.Protocol != "http" {
			return fmt.Errorf("%s.protocol: invalid", p)
		}
		if s.Protocol == "process" && s.Port != nil {
			return fmt.Errorf("%s.port: forbidden for process", p)
		}
		if s.Protocol != "process" && s.Port == nil {
			return fmt.Errorf("%s.port: required", p)
		}
		if s.Port != nil {
			switch v := s.Port.(type) {
			case int64:
				if v < 1 || v > 65535 {
					return fmt.Errorf("%s.port: must be 1..65535", p)
				}
			case string:
				if v != "auto" {
					return fmt.Errorf("%s.port: must be integer or auto", p)
				}
			default:
				return fmt.Errorf("%s.port: invalid type", p)
			}
		}
		depSeen := map[string]bool{}
		norm := map[string]string{}
		for _, dep := range s.DependsOn {
			if _, ok := c.Services[dep]; !ok {
				return fmt.Errorf("%s.depends_on: unknown service %q", p, dep)
			}
			if depSeen[dep] {
				return fmt.Errorf("%s.depends_on: duplicate %q", p, dep)
			}
			depSeen[dep] = true
			key := strings.ToUpper(regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(dep, "_"))
			if prev, ok := norm[key]; ok {
				return fmt.Errorf("%s.depends_on: %q conflicts with %q after environment normalization", p, dep, prev)
			}
			norm[key] = dep
		}
		for k, v := range s.Env {
			if !envRE.MatchString(k) || strings.HasPrefix(k, "RDR_") || strings.HasPrefix(k, "__RDR_") {
				return fmt.Errorf("%s.env.%s: invalid or reserved name", p, k)
			}
			if e := validateEnvFor(v, c, n); e != nil {
				return fmt.Errorf("%s.env.%s: %w", p, k, e)
			}
		}
		switch s.Restart {
		case "never", "on-failure", "always":
		default:
			return fmt.Errorf("%s.restart: expected never, on-failure, or always", p)
		}
		if s.RestartMaxAttempts <= 0 {
			return fmt.Errorf("%s.restart_max_attempts: must be positive", p)
		}
		if e := validateDurations(p, s); e != nil {
			return e
		}
		if e := validateHealth(p, s); e != nil {
			return e
		}
		if e := validateHooks(p, s); e != nil {
			return e
		}
		for hook, cmd := range s.Hooks {
			if e := validateCommandFor(cmd, c, n); e != nil {
				return fmt.Errorf("%s.hooks.%s: %w", p, hook, e)
			}
		}
		if e := validateRoutes(p, s, c); e != nil {
			return e
		}
		if e := validateShare(p, s, c, n); e != nil {
			return e
		}
		if s.Share != nil {
			if mode, _ := s.Share["mode"].(string); mode == "snapshot" {
				dir, _ := s.Share["dir"].(string)
				target := filepath.Join(cwd, dir)
				ancestor := target
				for {
					if _, er := os.Lstat(ancestor); er == nil {
						break
					}
					parent := filepath.Dir(ancestor)
					if parent == ancestor {
						return fmt.Errorf("%s.share.dir: no existing ancestor", p)
					}
					ancestor = parent
				}
				canon, er := filepath.EvalSymlinks(ancestor)
				if er != nil || !within(cwd, canon) {
					return fmt.Errorf("%s.share.dir: escapes cwd", p)
				}
			}
		}
		c.Services[n] = s
	}
	if cycle := findCycle(c.Services); cycle != "" {
		return fmt.Errorf("services: dependency cycle %s", cycle)
	}
	if c.Routing != nil {
		if c.Routing.Domain == "" {
			return fmt.Errorf("routing.domain: required")
		}
		if !validDomain(c.Routing.Domain) {
			return fmt.Errorf("routing.domain: invalid DNS domain")
		}
	}
	return nil
}

func validDomain(domain string) bool {
	if len(domain) > 253 {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}
func within(root, p string) bool {
	r, e := filepath.Rel(root, p)
	return e == nil && (r == "." || r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)))
}
func validateDurations(p string, s Service) error {
	if s.Health != nil {
		if raw, ok := s.Health["timeout"]; ok {
			v, ok := raw.(string)
			if !ok {
				return fmt.Errorf("%s.health.timeout: must be duration", p)
			}
			d, e := time.ParseDuration(v)
			if e != nil || d <= 0 {
				return fmt.Errorf("%s.health.timeout: must be positive duration", p)
			}
			start := s.StartTimeout
			if start == "" {
				start = "30s"
			}
			limit, e := time.ParseDuration(start)
			if e == nil && d > limit {
				return fmt.Errorf("%s.health.timeout: exceeds start_timeout", p)
			}
		}
	}
	for _, x := range []struct {
		k, v string
		d    string
	}{{"build_timeout", s.BuildTimeout, "5m"}, {"start_timeout", s.StartTimeout, "30s"}, {"stop_timeout", s.StopTimeout, "20s"}, {"hook_timeout", s.HookTimeout, "60s"}, {"restart_delay", s.RestartDelay, "1s"}, {"restart_max_delay", s.RestartMaxDelay, "30s"}, {"restart_stable_after", s.RestartStableAfter, "60s"}} {
		if x.v == "" {
			switch x.k {
			case "build_timeout":
				s.BuildTimeout = x.d
			case "start_timeout":
				s.StartTimeout = x.d
			case "stop_timeout":
				s.StopTimeout = x.d
			case "hook_timeout":
				s.HookTimeout = x.d
			}
			continue
		}
		d, e := time.ParseDuration(x.v)
		if e != nil || d <= 0 {
			return fmt.Errorf("%s.%s: must be positive duration", p, x.k)
		}
	}
	delay, _ := time.ParseDuration(s.RestartDelay)
	maxDelay, _ := time.ParseDuration(s.RestartMaxDelay)
	if delay > maxDelay {
		return fmt.Errorf("%s.restart_delay: exceeds restart_max_delay", p)
	}
	return nil
}
func validateHealth(p string, s Service) error {
	h := s.Health
	if h == nil {
		return nil
	}
	if len(h) == 0 {
		return fmt.Errorf("%s.health: empty table", p)
	}
	allowed := map[string]bool{"http": true, "tcp": true, "process": true, "timeout": true}
	n := 0
	for k, v := range h {
		if !allowed[k] {
			return fmt.Errorf("%s.health.%s: unknown field", p, k)
		}
		if k != "timeout" {
			n++
			switch k {
			case "http":
				x, ok := v.(string)
				if !ok || !strings.HasPrefix(x, "/") {
					return fmt.Errorf("%s.health.http: must be absolute path", p)
				}
			case "tcp", "process":
				if x, ok := v.(bool); !ok || !x {
					return fmt.Errorf("%s.health.%s: must be true", p, k)
				}
			}
		}
	}
	if n != 1 {
		return fmt.Errorf("%s.health: exactly one method required", p)
	}
	if _, ok := h["http"]; ok && s.Protocol != "http" {
		return fmt.Errorf("%s.health.http: requires http protocol", p)
	}
	if _, ok := h["tcp"]; ok && s.Port == nil {
		return fmt.Errorf("%s.health.tcp: requires port", p)
	}
	return nil
}
func validateHooks(p string, s Service) error {
	for k, v := range s.Hooks {
		switch k {
		case "before_start", "after_start", "before_stop", "after_stop":
		default:
			return fmt.Errorf("%s.hooks.%s: unknown hook", p, k)
		}
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s.hooks.%s: empty command", p, k)
		}
	}
	return nil
}
func validateRoutes(p string, s Service, c *Config) error {
	seen := map[string]bool{}
	for i, r := range s.Routes {
		q := fmt.Sprintf("%s.routes[%d]", p, i)
		if s.Protocol != "http" {
			return fmt.Errorf("%s: routes require http", q)
		}
		if !strings.HasPrefix(r.Path, "/") || strings.ContainsAny(r.Path, "?#") || seen[r.Path] || strings.TrimSpace(r.Dependency) == "" {
			return fmt.Errorf("%s.path: invalid or duplicate", q)
		}
		seen[r.Path] = true
		declared := false
		for _, dep := range s.DependsOn {
			if dep == r.Dependency {
				declared = true
			}
		}
		d, ok := c.Services[r.Dependency]
		if !declared || !ok || d.Protocol != "http" {
			return fmt.Errorf("%s.dependency: must name declared HTTP service", q)
		}
	}
	return nil
}
func validateShare(p string, s Service, c *Config, service string) error {
	v := s.Share
	if v == nil {
		return nil
	}
	if len(v) == 0 {
		return fmt.Errorf("%s.share: empty table", p)
	}
	allowed := map[string]bool{"mode": true, "auth": true, "build": true, "dir": true, "env": true}
	for k := range v {
		if !allowed[k] {
			return fmt.Errorf("%s.share.%s: unknown field", p, k)
		}
	}
	mode, _ := v["mode"].(string)
	if mode != "live" && mode != "snapshot" {
		return fmt.Errorf("%s.share.mode: must be live or snapshot", p)
	}
	if s.Protocol != "http" {
		return fmt.Errorf("%s.share: requires HTTP service", p)
	}
	if raw, ok := v["auth"]; ok {
		auth, yes := raw.(string)
		if !yes || auth != "password" {
			return fmt.Errorf("%s.share.auth: must be password", p)
		}
	}
	build, bok := v["build"].(string)
	if _, exists := v["build"]; exists && !bok {
		return fmt.Errorf("%s.share.build: must be string", p)
	}
	dir, dok := v["dir"].(string)
	if _, exists := v["dir"]; exists && !dok {
		return fmt.Errorf("%s.share.dir: must be string", p)
	}
	if raw, exists := v["env"]; exists {
		if mode != "snapshot" {
			return fmt.Errorf("%s.share.env: snapshot only", p)
		}
		env, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%s.share.env: must be table", p)
		}
		for k, x := range env {
			value, ok := x.(string)
			if !ok || !envRE.MatchString(k) || strings.HasPrefix(k, "RDR_") || strings.HasPrefix(k, "__RDR_") {
				return fmt.Errorf("%s.share.env.%s: invalid key or value", p, k)
			}
			if e := validateEnvFor(value, c, service); e != nil {
				return fmt.Errorf("%s.share.env.%s: %w", p, k, e)
			}
		}
	}
	if mode == "snapshot" {
		if e := validateCommandFor(build, c, service); e != nil {
			return fmt.Errorf("%s.share.build: %w", p, e)
		}
	}
	if mode == "snapshot" && !filepath.IsLocal(dir) {
		return fmt.Errorf("%s.share.dir: must be relative", p)
	}
	if mode == "snapshot" && (strings.TrimSpace(build) == "" || strings.TrimSpace(dir) == "") {
		return fmt.Errorf("%s.share: snapshot requires build and dir", p)
	}
	if mode == "live" && (build != "" || dir != "") {
		return fmt.Errorf("%s.share: build/dir forbidden for live", p)
	}
	return nil
}
func findCycle(m map[string]Service) string {
	state := map[string]bool{}
	done := map[string]bool{}
	var walk func(string) string
	walk = func(n string) string {
		if state[n] {
			return n
		}
		if done[n] {
			return ""
		}
		state[n] = true
		for _, d := range m[n].DependsOn {
			if x := walk(d); x != "" {
				return n + " -> " + x
			}
		}
		state[n] = false
		done[n] = true
		return ""
	}
	for n := range m {
		if x := walk(n); x != "" {
			return x
		}
	}
	return ""
}

func validateEmptyFields(root map[string]any) error {
	services, _ := root["services"].(map[string]any)
	for name, value := range services {
		s, ok := value.(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"build", "cwd", "protocol", "build_timeout", "start_timeout", "stop_timeout", "hook_timeout", "restart", "restart_delay", "restart_max_delay", "restart_stable_after"} {
			if v, exists := s[field]; exists {
				if text, ok := v.(string); ok && strings.TrimSpace(text) == "" {
					return fmt.Errorf("services.%s.%s: must not be empty", name, field)
				}
			}
		}
		if attempts, ok := s["restart_max_attempts"].(int64); ok && attempts <= 0 {
			return fmt.Errorf("services.%s.restart_max_attempts: must be positive", name)
		}
		if raw, exists := s["share"]; exists {
			if share, ok := raw.(map[string]any); ok {
				if v, present := share["build"]; present {
					if text, ok := v.(string); !ok || strings.TrimSpace(text) == "" {
						return fmt.Errorf("services.%s.share.build: must not be empty", name)
					}
				}
				if v, present := share["dir"]; present {
					if text, ok := v.(string); !ok || strings.TrimSpace(text) == "" {
						return fmt.Errorf("services.%s.share.dir: must not be empty", name)
					}
				}
			}
		}
	}
	return nil
}
