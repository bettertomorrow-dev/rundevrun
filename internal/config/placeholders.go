// Parses and validates command placeholders.
package config

import (
	"fmt"
	"strings"
)

var ErrVersionUnsupported = fmt.Errorf("unsupported config version")

type Error struct {
	Code, Field string
	Err         error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Field + ": " + e.Err.Error()
	}
	return e.Field + ": invalid"
}
func (e *Error) Unwrap() error { return e.Err }
func scan(s string, command bool, c *Config) (bool, error) {
	seen := false
	quote := byte(0)
	for i := 0; i < len(s); {
		ch := s[i]
		if command {
			if quote == 0 && ch == '\'' {
				quote = '\''
				i++
				continue
			}
			if quote == '\'' && ch == '\'' {
				quote = 0
				i++
				continue
			}
			if quote == 0 && ch == '"' {
				quote = '"'
				i++
				continue
			}
			if quote == '"' && ch == '"' {
				quote = 0
				i++
				continue
			}
			if quote == 0 && ch == '\\' {
				return false, fmt.Errorf("unquoted backslash is forbidden")
			}
			if quote == 0 && ch == '#' {
				return false, fmt.Errorf("comments are forbidden with placeholders")
			}
			if quote != '\'' && ch == '`' {
				return false, fmt.Errorf("backticks are forbidden")
			}
			if quote != '\'' && strings.HasPrefix(s[i:], "$(") {
				return false, fmt.Errorf("command substitution is forbidden")
			}
			if quote != '\'' && strings.HasPrefix(s[i:], "<<") {
				return false, fmt.Errorf("heredocs are forbidden")
			}
		}
		if strings.HasPrefix(s[i:], "${") {
			j := strings.IndexByte(s[i+2:], '}')
			if j < 0 {
				return false, fmt.Errorf("unclosed shell variable")
			}
			v := s[i+2 : i+2+j]
			for _, r := range v {
				if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
					return false, fmt.Errorf("only plain shell variables are allowed")
				}
			}
			i += j + 3
			continue
		}
		if strings.HasPrefix(s[i:], "{{") {
			j := strings.Index(s[i+2:], "}}")
			if j < 0 {
				return false, fmt.Errorf("unmatched literal brace")
			}
			i += j + 4
			continue
		}
		if strings.HasPrefix(s[i:], "}}") {
			i += 2
			continue
		}
		if ch == '{' {
			j := strings.IndexByte(s[i+1:], '}')
			if j < 0 {
				return false, fmt.Errorf("unmatched brace")
			}
			name := s[i+1 : i+1+j]
			if !validPlaceholder(name, c) {
				return false, fmt.Errorf("unknown or unavailable placeholder {%s}", name)
			}
			if command && quote != 0 {
				return false, fmt.Errorf("placeholder {%s} must be unquoted", name)
			}
			seen = true
			i += j + 2
			continue
		}
		if ch == '}' {
			return false, fmt.Errorf("unmatched brace")
		}
		i++
	}
	if quote != 0 {
		return false, fmt.Errorf("unbalanced quote")
	}
	return seen, nil
}
func validPlaceholder(n string, c *Config) bool {
	switch n {
	case "host", "port", "direct_url", "url", "run_dir", "worktree", "project", "service":
		return true
	}
	if strings.HasPrefix(n, "dependencies.") {
		p := strings.Split(n, ".")
		if len(p) != 3 {
			return false
		}
		if _, ok := c.Services[p[1]]; !ok {
			return false
		}
		switch p[2] {
		case "host", "port", "direct_url", "url":
			return true
		}
	}
	return false
}

func validateCommandFor(text string, c *Config, service string) error {
	if !strings.Contains(strings.ReplaceAll(text, "${", ""), "{") {
		return nil
	}
	_, e := scanFor(text, true, c, service)
	return e
}
func validateEnvFor(text string, c *Config, service string) error {
	_, e := scanFor(text, false, c, service)
	return e
}
func scanFor(text string, command bool, c *Config, service string) (bool, error) {
	owner, ok := c.Services[service]
	if !ok {
		return false, fmt.Errorf("unknown service %q", service)
	}
	allowed := map[string]bool{}
	for _, d := range owner.DependsOn {
		allowed[d] = true
	}
	for i := 0; i < len(text); {
		if strings.HasPrefix(text[i:], "${") {
			j := strings.IndexByte(text[i+2:], '}')
			if j < 0 {
				break
			}
			i += j + 3
			continue
		}
		if strings.HasPrefix(text[i:], "{{") {
			j := strings.Index(text[i+2:], "}}")
			if j < 0 {
				break
			}
			i += j + 4
			continue
		}
		if text[i] == '{' {
			j := strings.IndexByte(text[i+1:], '}')
			if j < 0 {
				break
			}
			name := text[i+1 : i+1+j]
			if strings.HasPrefix(name, "dependencies.") {
				parts := strings.Split(name, ".")
				if len(parts) != 3 || !allowed[parts[1]] {
					return false, fmt.Errorf("%s is not a declared dependency", name)
				}
				dep := c.Services[parts[1]]
				if parts[2] == "port" && dep.Port == nil {
					return false, fmt.Errorf("%s is unavailable without a port", name)
				}
				if (parts[2] == "url" || parts[2] == "direct_url") && dep.Protocol != "http" {
					return false, fmt.Errorf("%s is unavailable for non-HTTP service", name)
				}
			} else {
				switch name {
				case "port":
					if owner.Port == nil {
						return false, fmt.Errorf("{%s} is unavailable without a port", name)
					}
				case "url", "direct_url":
					if owner.Protocol != "http" {
						return false, fmt.Errorf("{%s} is unavailable for non-HTTP service", name)
					}
				}
			}
			i += j + 2
			continue
		}
		i++
	}
	return scan(text, command, c)
}
