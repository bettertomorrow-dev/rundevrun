// Expands service placeholders through shell environment variables.
package runner

import (
	"fmt"
	"strings"
)

func substitute(input string, values map[string]string, command bool) (string, map[string]string, error) {
	if command && !hasPlaceholder(input, values) {
		return strings.ReplaceAll(strings.ReplaceAll(input, "{{", "{"), "}}", "}"), map[string]string{}, nil
	}
	var out strings.Builder
	env := map[string]string{}
	for i := 0; i < len(input); {
		if strings.HasPrefix(input[i:], "${") {
			j := strings.IndexByte(input[i+2:], '}')
			if j < 0 {
				return "", nil, fmt.Errorf("unclosed shell variable")
			}
			end := i + j + 3
			out.WriteString(input[i:end])
			i = end
			continue
		}
		if strings.HasPrefix(input[i:], "{{") {
			out.WriteByte('{')
			i += 2
			continue
		}
		if strings.HasPrefix(input[i:], "}}") {
			out.WriteByte('}')
			i += 2
			continue
		}
		if input[i] == '{' {
			j := strings.IndexByte(input[i+1:], '}')
			if j < 0 {
				return "", nil, fmt.Errorf("unmatched brace")
			}
			name := input[i+1 : i+1+j]
			value, ok := values[name]
			if !ok {
				return "", nil, fmt.Errorf("unavailable placeholder {%s}", name)
			}
			if command {
				key := fmt.Sprintf("__RDR_SUB_%d", len(env))
				env[key] = value
				out.WriteString(`"${` + key + `}"`)
			} else {
				out.WriteString(value)
			}
			i += j + 2
			continue
		}
		if input[i] == '}' {
			return "", nil, fmt.Errorf("unmatched brace")
		}
		out.WriteByte(input[i])
		i++
	}
	return out.String(), env, nil
}

func hasPlaceholder(input string, values map[string]string) bool {
	for i := 0; i < len(input); i++ {
		if strings.HasPrefix(input[i:], "${") {
			j := strings.IndexByte(input[i+2:], '}')
			if j < 0 {
				return false
			}
			i += j + 2
			continue
		}
		if strings.HasPrefix(input[i:], "{{") {
			i++
			continue
		}
		if input[i] == '{' {
			j := strings.IndexByte(input[i+1:], '}')
			if j < 0 {
				continue
			}
			name := input[i+1 : i+1+j]
			_, ok := values[name]
			switch name {
			case "host", "port", "url", "direct_url", "run_dir", "worktree", "project", "service":
				ok = true
			}
			if strings.HasPrefix(name, "dependencies.") {
				ok = true
			}
			if ok {
				return true
			}
			i += j + 1
		}
	}
	return false
}
