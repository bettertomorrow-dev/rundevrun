# rdrtop

`rdrtop` is a separate terminal view of services managed by rundevrun. Closing
it leaves services running.

The interface uses the same stack as TlgMe: Bubble Tea for terminal events and
asynchronous commands, Bubbles for the scrolling viewport, and Lip Gloss for
styling. It runs in the alternate screen and restores the terminal on exit.

```sh
go build -trimpath -o /tmp/rdrtop ./cmd/rdrtop
/tmp/rdrtop
```

The default view shows running services. Press Tab for each service's latest
unresolved launch error. A successful launch of that service clears its error.
The initial tab follows the latest launch result in the current
project, or across all projects when started outside a registered project.
Inside a project, press `a` to show every project. Services are grouped by
project and show their worktree so matching service names remain distinct.

Use the arrow keys or `j` and `k` to select a service. In the log view, use
those keys or PgUp/PgDown to scroll. Logs and service state refresh once per
second. Press `l` for logs, `o`
to open its URL, `s` to stop it, or `q` to quit. A live service whose worktree
was removed stays visible and can be stopped with the action labelled
"Stop process without worktree". This action closes attached publications and
skips stop hooks when the worktree directory no longer exists, after verifying
and cleaning up any recorded hook processes. Normal stops run the saved hooks.
Reads do not register projects or modify runtime state.

The first normal `rdr up` remembers a project after validating its configuration,
including when a later build or launch fails. `rdr register` still supports
explicit registration. `rdr up --dry-run` writes no state. Both binaries use
`RDR_HOME`, then `XDG_STATE_HOME/rundevrun`, then `~/.local/state/rundevrun`.
No additional daemon is needed. Displayed ports are allocated service ports;
readiness is shown separately in the service state.

An application can print `app: url=https://example.test/path/` on its own line
to announce the URL to display and open with `o`. For example, twikko prints
`twikko: url=http://127.0.0.1:20002/s/example/`. The latest valid HTTP or HTTPS
announcement in the current run's retained logs takes precedence over the
generated service URL. Without an announcement, the generated URL is used.
URLs containing credentials are ignored. Announcements from previous runs are
not used, and no application restart is needed to read existing logs.

Verification on macOS or Linux:

```sh
go test ./...
go test -race ./...
go vet ./...
python3 scripts/rdrtop_smoke.py
```

The acceptance test starts isolated HTTP services in two projects and two
worktrees, checks a failed build, deletes a worktree, and stops its remaining
process. The PTY smoke test checks logs, terminal restoration, service survival
after quitting, and stopping through the TUI with hooks.
