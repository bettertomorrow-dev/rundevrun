// Package cli implements the noninteractive rdr command line.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const help = `rdr manages development services configured in dev.toml.

Discovery and registration:
  rdr register [path]     Register a project and its existing worktrees.
  rdr projects            List registered projects and their stable IDs.
  rdr worktrees           List existing worktrees; never change Git state.
  rdr services            List configured services, including stopped instances.
  rdr config check        Validate configuration without running project code.
  rdr up <service>        Build and start one service.
  rdr down <service>      Stop one service.
  rdr restart <service>   Restart one service.
  rdr logs <service>      Show recent application output.
  rdr ls                  Show known instances across registered projects.
  rdr status              Show known instances, optionally scoped to a project.

Options may appear before or after the command:
  --project ID|NAME       Select a registered project. Ambiguity is an error.
  --worktree ID|PATH|NAME Select an existing worktree.
  --json                  Write one versioned JSON result to stdout.
  --dry-run               Show a lifecycle plan without changing state.
  --tail N                Show the last N log lines, default 100, maximum 10000.
  --follow                Follow application logs; cannot combine with --json.
  --help, -h              Show this help.

Examples:
  rdr register . --json
  rdr services --project example --worktree ./feature --json
  rdr config check --json
  rdr up api --dry-run --json
  rdr up api --json
  rdr logs api --tail 50
  rdr down api --json

Lifecycle currently accepts one service and rejects dependencies and hooks.
Proxy, sharing and graph operations remain in progress.
Runtime state: RDR_HOME, else XDG_STATE_HOME/rundevrun,
else ~/.local/state/rundevrun. Registration runs no project commands.`

func Run(ctx context.Context, args []string, out, errOut io.Writer) int {
	opts, positional, err := parseArgs(args)
	// Even a usage error preceding --json must retain machine-readable output.
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--json" {
			opts.json = true
		}
	}
	command := "help"
	if len(positional) > 0 {
		command = positional[0]
		if command == "config" && len(positional) > 1 {
			command += " " + positional[1]
		}
	}
	e := result(command, opts)
	if err != nil {
		e.fail("USAGE", err.Error(), "see rdr --help")
		return writeResult(out, errOut, e, opts.json)
	}
	if opts.help || command == "help" {
		if opts.json {
			e.Results = append(e.Results, map[string]string{"help": help})
			return writeResult(out, errOut, e, true)
		}
		if _, err := fmt.Fprintln(out, help); err != nil {
			fmt.Fprintln(errOut, "rdr: cannot write help:", err)
			return 4
		}
		return 0
	}
	if err := ctx.Err(); err != nil {
		e.fail("CANCELLED", err.Error())
		return writeResult(out, errOut, e, opts.json)
	}
	execute(ctx, positional, opts, &e, out)
	return writeResult(out, errOut, e, opts.json)
}

func runtimeRoot() (string, error) {
	if root := os.Getenv("RDR_HOME"); root != "" {
		return filepath.Abs(root)
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Abs(filepath.Join(base, "rundevrun"))
}

func validArity(args []string, min, max int, e *envelope) bool {
	if len(args) < min || len(args) > max {
		e.fail("USAGE", "unexpected arguments: "+strings.Join(args, " "), "see rdr --help")
		return false
	}
	return true
}
