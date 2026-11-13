package cli

import (
	"fmt"
	"strconv"
	"strings"
)

type options struct {
	project    string
	worktree   string
	toWorktree string
	all        bool
	cascade    bool
	json       bool
	dryRun     bool
	help       bool
	follow     bool
	tail       int
	tailSet    bool
}

// Common flags may precede or follow the command and selectors.
func parseArgs(args []string) (options, []string, error) {
	opts := options{tail: 100}
	var positional []string
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if seen[name] {
			return opts, positional, fmt.Errorf("duplicate flag %s", name)
		}
		seen[name] = true
		switch name {
		case "--json", "--dry-run", "--help", "-h", "--follow", "--all", "--cascade":
			if hasValue {
				return opts, positional, fmt.Errorf("%s does not take a value", name)
			}
			switch name {
			case "--json":
				opts.json = true
			case "--dry-run":
				opts.dryRun = true
			case "--follow":
				opts.follow = true
			case "--all":
				opts.all = true
			case "--cascade":
				opts.cascade = true
			default:
				opts.help = true
			}
		case "--project", "--worktree", "--to-worktree", "--tail":
			if !hasValue {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return opts, positional, fmt.Errorf("%s requires a value", name)
				}
				i++
				value = args[i]
			}
			if value == "" {
				return opts, positional, fmt.Errorf("%s requires a nonempty value", name)
			}
			if name == "--tail" {
				n, err := strconv.Atoi(value)
				if err != nil || n < 0 || n > 10000 {
					return opts, positional, fmt.Errorf("--tail requires an integer from 0 to 10000")
				}
				opts.tail, opts.tailSet = n, true
			} else if name == "--to-worktree" {
				opts.toWorktree = value
			} else if name == "--project" {
				opts.project = value
			} else {
				opts.worktree = value
			}
		default:
			return opts, positional, fmt.Errorf("unknown flag %s", name)
		}
	}
	return opts, positional, nil
}
