# Local build and use

`rundevrun` is an intermediate CLI, not a release. Build it from a checkout
with Go 1.27 or newer and Git installed. Use `rdr version --json` for the product
and public API versions. Record the checkout revision and `go version` alongside
a local build.

## Build and install

From the repository root:

```sh
go version
go build -trimpath -o /tmp/rdr ./cmd/rdr
/tmp/rdr version --json
/tmp/rdr --help
```

Install a user-owned copy without changing system directories or requiring
`sudo`:

```sh
install -d -m 0700 "$HOME/.local/opt/rundevrun"
install -m 0755 /tmp/rdr "$HOME/.local/opt/rundevrun/rdr"
"$HOME/.local/opt/rundevrun/rdr" --help
```

Use that absolute path in commands, or choose another private user-owned install
directory. `version --json` returns `results[0].api_version`, currently `1`, for project
wrappers to check compatibility; `--help` lists the available commands.

## First project

Put a `dev.toml` in the project root, then inspect it without running project
commands:

```sh
RDR="$HOME/.local/opt/rundevrun/rdr"
"$RDR" config check --json
"$RDR" register . --dry-run --json
```

`config check` validates the current project configuration. `register --dry-run`
shows the discovered project and worktrees without storing registration state.
After reviewing the configuration and plan, register it and inspect the public
JSON views:

```sh
"$RDR" register . --json
"$RDR" projects --json
"$RDR" worktrees --project PROJECT_ID --json
"$RDR" services --project PROJECT_ID --worktree WORKTREE_ID --json
"$RDR" status --project PROJECT_ID --json
```

Use the stable IDs returned by JSON when a name or path could be ambiguous.
`status`, `ls`, `projects`, `worktrees`, and `services` report observed state;
they do not start services. `up --dry-run` shows a lifecycle plan. `up` runs
commands from `dev.toml`, so review those commands before starting them.

```sh
"$RDR" up api --project PROJECT_ID --worktree WORKTREE_ID --dry-run --json
"$RDR" up api --project PROJECT_ID --worktree WORKTREE_ID --json
"$RDR" logs api --project PROJECT_ID --worktree WORKTREE_ID --tail 50
"$RDR" open api --project PROJECT_ID --worktree WORKTREE_ID --json
"$RDR" doctor --project PROJECT_ID --worktree WORKTREE_ID --json
"$RDR" down api --project PROJECT_ID --worktree WORKTREE_ID --json
```

`open --json` reports the URL without launching a browser. Text-mode `open`
uses the platform browser opener. `doctor` reads configuration and current
diagnostics. For dependencies, use `bind` to change the desired worktree and
`restart` to apply the new selection. `switch` moves a live service and its
consumers to a target worktree. Both support public JSON planning with
`--dry-run`; see `--help` for their selectors.

## State and credentials

The project file is `dev.toml`. Machine routing configuration is
`$XDG_CONFIG_HOME/rundevrun/config.toml`, or `~/.config/rundevrun/config.toml`
when `XDG_CONFIG_HOME` is unset. Private runtime state is stored under
`$RDR_HOME` when set, otherwise `$XDG_STATE_HOME/rundevrun`, or
`~/.local/state/rundevrun` when `XDG_STATE_HOME` is unset. Keep those state
directories private and backed up only as appropriate for local development.
Application data belongs in a separate project directory outside `RDR_HOME`;
runner retention may remove old build artifacts. Do not copy runtime files into
project repositories or publish them as logs.

Creating a live or snapshot share returns its password once, including in the
JSON response. Treat that command output as a secret. A repeated share and
status views do not return the password. Recreate rotates the share URL and
password; it does not open the URL automatically. Quick Tunnels do not support
SSE. For local HTTPS setup and external acceptance limits, read
[LOCAL_HTTPS.md](LOCAL_HTTPS.md) and [ACCEPTANCE.md](ACCEPTANCE.md).

## Verification

See [verification and known limits](ACCEPTANCE.md) for reproducible checks.
