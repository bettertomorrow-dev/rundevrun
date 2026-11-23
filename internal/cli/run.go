// Package cli implements the noninteractive rundevrun command line.
package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
)

const help = `rundevrun manages development services configured in rundevrun.toml.

Discovery and registration:
  rundevrun guide                 List available guides.
  rundevrun guide onboard [--no-update]
                                  Retrieve project onboarding instructions.
  rundevrun guide domain [--no-update]
                                  Retrieve private HTTPS setup and recovery instructions.
  rundevrun version               Show the binary version and public API version.
  rundevrun register [path]       Register a project and its existing worktrees.
  rundevrun projects              List registered projects and their stable IDs.
  rundevrun worktrees             List existing worktrees; never change Git state.
  rundevrun services              List configured services, including stopped instances.
  rundevrun config check          Validate configuration without running project code.
  rundevrun config state          Show the absolute runtime state directory without changing it.
  rundevrun config migrate-state  Stop legacy processes and migrate registrations.
  rundevrun config external [path] [--file PATH]
                                  Show the external config path, or import a config there.
  rundevrun up <service...>       Build and start services and their dependencies.
  rundevrun down [service...]     Stop services; without names, select this worktree.
  rundevrun down --all            Select services across all registered projects.
  rundevrun restart <service...> Restart selected services; start missing dependencies.
  rundevrun switch <service...> --project ID --worktree SOURCE --to-worktree TARGET
                                  Move live services and their consumers to another worktree.
  rundevrun switch --all-services --project ID --worktree SOURCE --to-worktree TARGET
                                  Switch live services in one source worktree.
  rundevrun switch --file path    Read version 1 mappings from a JSON file.
  rundevrun bind <service> <dependency> --to-worktree ID|PATH|NAME
                                  Set a desired dependency binding; live services need restart.
  rundevrun alias LABEL           Name each service route in one worktree.
  rundevrun alias SERVICE HOST    Assign one HTTP service a full hostname.
  rundevrun open <service>        Open a registered route from the current worktree.
  rundevrun proxy status|reload|stop|abort
                                  Inspect or control the local routing proxy. abort releases a pending listener reservation.
  rundevrun doctor                Diagnose routes, TLS, proxy, and application state.
  rundevrun network inspect|check|plan|apply
                                  Inspect or change networking settings.
  rundevrun network setup-check|setup-plan|setup-apply|credential-store
                                  Check and configure tailnet access. setup-plan contacts providers
                                  without changes; setup-apply applies the reviewed plan.
  rundevrun network cleanup-plan
                                  Preview release of owned DNS and Serve resources.
  rundevrun network cleanup-apply --file PLAN.json
                                  Release the resources described by a reviewed plan.
  rundevrun network setup-recover|resolve-finalization|adopt-dns|adopt-existing-dns|adopt-serve|resolve-challenge
                                  Recover owned resources or explicitly adopt conflicts.
  rundevrun network credential-replace REF
                                  Replace a stored Cloudflare credential using stdin.
  rundevrun share <service>       Create a live public link for one HTTP service.
  rundevrun unshare <service>     Remove its public link without stopping the service.
  rundevrun logs <service>        Show recent application output.
  rundevrun status                Show known instances and their URLs across registered projects.

Options may appear before or after the command:
  --project ID|NAME       Select a registered project. Ambiguity is an error.
  --worktree ID|PATH|NAME Select an existing worktree.
  --to-worktree ID|PATH|NAME Select bind target or switch destination.
  --file PATH              Read a config to import, switch mappings, or a network plan.
  --all-services           Select live source services for switch.
  --no-update             Skip the guide update check.
  --json                  Write one versioned JSON result to stdout.
  --dry-run               Show a lifecycle plan without changing state.
  --cascade               Include live consumers in a down plan.
  --all                   Global down scope; cannot combine with selectors.
  --retry-hooks           Retry a saved failed or interrupted stop hook.
  --no-proxy              Deprecated; local routing remains enabled.
  --mode live|snapshot    Select a live link or configured static snapshot.
  --recreate              Replace an existing share after the new link is ready.
  --tail N                Show the last N log lines, default 100, maximum 10000.
  --follow                Follow application logs; cannot combine with --json.
  --help, -h              Show this help.

Examples:
  rundevrun register . --json
  rundevrun services --project example --worktree ./feature --json
  rundevrun config check --json
  rundevrun config external --file /tmp/rundevrun.toml --json
  rundevrun up api --dry-run --json
  rundevrun up api --json
  rundevrun logs api --tail 50
  rundevrun open console --project example --worktree ./feature-a --json
  rundevrun doctor --project example --json
  rundevrun down api --json

Graph dry-runs support service lists, default down, and global down. Retry uses
the original stopped run's hook record; a new consumer does not retry a failed
after_start hook.
--no-proxy is deprecated. Local routing remains enabled.
If proxy reload reports a TLS mode or listener change requires restart, run
rundevrun proxy stop, then rundevrun up or reopen rundevtop to start the proxy again.
Sharing requires a running HTTP service. The password is shown once at creation;
recreate replaces an existing link. rundevrun does not open the URL automatically.
Quick Tunnels do not support server-sent events (SSE).
Status shows switch journals without replay. Interrupted switches
are not rolled back or retried automatically. Inspect the steps, then use up or
down to recover explicitly. A failed after_start hook remains attached to its run.
Runtime state uses RDR_HOME when set, or defaults to XDG_CONFIG_HOME/rundevrun/state
(normally ~/.config/rundevrun/state). Running up, down, restart, or proxy stop
migrates legacy state first and stops its services and proxy.
Registration runs no project commands.`

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
		if len(positional) > 1 {
			switch command {
			case "config", "proxy", "network", "guide":
				command += " " + positional[1]
			}
		}
		if positional[0] == "guide" && !opts.help {
			opts.json = true
		}
	}
	e := result(command, opts)
	if command == "up" && !opts.json && !opts.dryRun {
		e.progress = errOut
	}
	if err != nil {
		e.fail("USAGE", err.Error(), "see rundevrun --help")
		return writeResult(out, errOut, e, opts.json)
	}
	if opts.help || command == "help" {
		if opts.json {
			e.Results = append(e.Results, map[string]string{"help": help})
			return writeResult(out, errOut, e, true)
		}
		if _, err := fmt.Fprintln(out, help); err != nil {
			fmt.Fprintln(errOut, "rundevrun: cannot write help:", err)
			return 4
		}
		return 0
	}
	if err := ctx.Err(); err != nil {
		e.fail("CANCELLED", err.Error())
		return writeResult(out, errOut, e, opts.json)
	}
	progress := errOut
	if opts.json {
		progress = io.Discard
	}
	execute(ctx, positional, opts, &e, out, progress)
	return writeResult(out, errOut, e, opts.json)
}

func validArity(args []string, min, max int, e *envelope) bool {
	if len(args) < min || len(args) > max {
		e.fail("USAGE", "unexpected arguments: "+strings.Join(args, " "), "see rundevrun --help")
		return false
	}
	return true
}
