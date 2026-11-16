// Validates and constructs local hostnames.
package routing

import (
	"errors"
	"fmt"
	"strings"
)

func validateDNS(host string) (string, error) {
	if len(host) == 0 || len(host) > 253 {
		return "", errors.New("hostname is empty or too long; set an explicit alias")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid DNS label; set an explicit alias")
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return "", errors.New("invalid DNS label; set an explicit alias")
			}
		}
	}
	return host, nil
}
func label(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func (c Config) Hostname(service, worktree, project, domain string) (string, error) {
	if domain == "" {
		domain = c.Routing.Domain
	}
	domain, err := validateDNS(strings.ToLower(domain))
	if err != nil {
		return "", fmt.Errorf("domain: %w", err)
	}
	values := []string{label(service), label(worktree), label(project)}
	for _, v := range values {
		if v == "" {
			return "", errors.New("empty hostname component; set an explicit alias")
		}
	}
	template := c.Routing.Hostname
	if template == "" {
		return "", errors.New("hostname template is empty")
	}
	if !strings.HasSuffix(template, ".{domain}") || strings.Count(template, "{domain}") != 1 {
		return "", errors.New("hostname template must end in .{domain}")
	}
	prefix := strings.TrimSuffix(template, ".{domain}")
	prefix = strings.NewReplacer("{service}", values[0], "{worktree}", values[1], "{project}", values[2]).Replace(prefix)
	if strings.ContainsAny(prefix, ".{}") {
		return "", errors.New("hostname template must produce one label using known tokens")
	}
	return validateDNS(strings.ToLower(prefix) + "." + domain)
}
