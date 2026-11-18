# Installation

Requires Go 1.27 or newer and Git. From the repository root:

```sh
go build -trimpath -o /tmp/rundevrun ./cmd/rundevrun
install -d -m 0700 "$HOME/.local/opt/rundevrun"
install -m 0755 /tmp/rundevrun "$HOME/.local/opt/rundevrun/rundevrun"
"$HOME/.local/opt/rundevrun/rundevrun" version --json
```

Add `$HOME/.local/opt/rundevrun` to PATH or use the absolute path.
`version --json` reports product and API versions; `--help` lists commands.
Install the optional terminal app from the same checkout:

```sh
go build -trimpath -o /tmp/rundevtop ./cmd/rundevtop
install -m 0755 /tmp/rundevtop "$HOME/.local/opt/rundevrun/rundevtop"
```

The [README](../README.md) shows optional `rdr` and `rdrtop` shell aliases.
The executable rename keeps existing runtime state and registrations, including
the `RDR_HOME` environment setting. Use `rundevrun` in scripts that launch it.

## First project

Create the [example configuration](../README.md), then run from its directory:

```sh
rundevrun config check --json
rundevrun up api --dry-run --json
rundevrun up api --json
rundevrun status --json
rundevrun open api --json
rundevrun down api --json
```

Review project commands before `up`. It registers the project automatically;
`rundevrun register .` supports explicit registration. `open --json` returns the URL;
text-mode `open` launches a browser. Use `doctor` for diagnostics and `--project`
and `--worktree` to select services outside the current checkout.

Use `bind` followed by `restart` to change a dependency's worktree. `switch`
moves a live service and its consumers. Inspect these operations with `--dry-run`.

## State and secrets

Runtime state uses `$RDR_HOME`, then `$XDG_STATE_HOME/rundevrun`, then
`~/.local/state/rundevrun`. Keep application data elsewhere; retention removes
old run artifacts. Do not commit runtime files or publish their logs.

Machine networking settings use `$XDG_CONFIG_HOME/rundevrun`, falling back to
`~/.config/rundevrun`. See [networking](NETWORKING.md) and [existing TLS](LOCAL_HTTPS.md).

Share creation returns its password once, including in JSON. Treat that output
as secret. Repeated shares and status do not return it; `--recreate` rotates the
URL and password. Quick Tunnels do not support SSE.
