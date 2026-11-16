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
  rdr version             Show the binary version and public API version.
  rdr register [path]     Register a project and its existing worktrees.
  rdr projects            List registered projects and their stable IDs.
  rdr worktrees           List existing worktrees; never change Git state.
  rdr services            List configured services, including stopped instances.
  rdr config check        Validate configuration without running project code.
  rdr up <service...>     Build and start services and their dependencies.
  rdr down [service...]   Stop services; without names, select this worktree.
  rdr down --all          Select services across all registered projects.
  rdr restart <service...> Restart selected services; start missing dependencies.
  rdr switch <service...> --project ID --worktree SOURCE --to-worktree TARGET
                          Move live services and their consumers to another worktree.
  rdr switch --all-services --project ID --worktree SOURCE --to-worktree TARGET
                          Switch live services in one source worktree.
  rdr switch --file path  Read version 1 mappings from a JSON file.
  rdr bind <service> <dependency> --to-worktree ID|PATH|NAME
                          Set a desired dependency binding; live services need restart.
  rdr alias LABEL         Name each service route in one worktree.
  rdr alias SERVICE HOST  Assign one HTTP service a full hostname.
  rdr open <service>     Open a registered route from the current worktree.
  rdr proxy status|reload|stop
                          Inspect or control the local routing proxy.
  rdr doctor             Diagnose routes, TLS, proxy, and application state.
  rdr share <service>    Create a live public link for one HTTP service.
  rdr unshare <service>  Remove its public link without stopping the service.
  rdr logs <service>      Show recent application output.
  rdr ls                  Show known instances across registered projects.
  rdr status              Show known instances, optionally scoped to a project.

Options may appear before or after the command:
  --project ID|NAME       Select a registered project. Ambiguity is an error.
  --worktree ID|PATH|NAME Select an existing worktree.
  --to-worktree ID|PATH|NAME Select bind target or switch destination.
  --file PATH              Read switch mappings from a JSON file.
  --all-services           Select live source services for switch.
  --json                  Write one versioned JSON result to stdout.
  --dry-run               Show a lifecycle plan without changing state.
  --cascade               Include live consumers in a down plan.
  --all                   Global down scope; cannot combine with selectors.
  --retry-hooks           Retry a saved failed or interrupted stop hook.
  --no-proxy              Skip automatic proxy start/reload after up, restart, or switch.
  --mode live|snapshot    Select a live link or configured static snapshot.
  --recreate              Replace an existing share after the new link is ready.
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
  rdr open console --project example --worktree ./feature-a --json
  rdr doctor --project example --json
  rdr down api --json

Graph dry-runs support service lists, default down, and global down. Retry uses
the original stopped run's hook record; a new consumer does not retry a failed
after_start hook.
--no-proxy skips only automatic proxy startup; routed addresses remain planned and direct URLs stay available.
Sharing requires a running HTTP service. The password is shown once at creation;
recreate replaces an existing link. rdr does not open the URL automatically.
Quick Tunnels do not support server-sent events (SSE).
Switch journals are shown by status and ls without replay. Interrupted switches
are not rolled back or retried automatically. Inspect the steps, then use up or
down to recover explicitly. A failed after_start hook remains attached to its run.
Runtime state: RDR_HOME, else XDG_STATE_HOME/rundevrun,
else ~/.local/state/rundevrun. Registration runs no project commands.`

func Run(ctx context.Context, args []string, out, errOut io.Writer) int {
	opts, positional, err := parseArgs(args)

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
		if command == "proxy" && len(positional) > 1 {
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
