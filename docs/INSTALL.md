# Installation

Requires Go 1.27 or newer and Git. From the repository root:

```sh
go build -trimpath -o /tmp/rdr ./cmd/rdr
install -d -m 0700 "$HOME/.local/opt/rundevrun"
install -m 0755 /tmp/rdr "$HOME/.local/opt/rundevrun/rdr"
"$HOME/.local/opt/rundevrun/rdr" version --json
```

Add `$HOME/.local/opt/rundevrun` to PATH or use the absolute path.
`version --json` reports product and API versions; `--help` lists commands.
Build the optional terminal app with `go build -o /tmp/rdrtop ./cmd/rdrtop`.

## First project

Create the [example configuration](../README.md), then run from its directory:

```sh
rdr config check --json
rdr up api --dry-run --json
rdr up api --json
rdr status --json
rdr open api --json
rdr down api --json
```

Review project commands before `up`. It registers the project automatically;
`rdr register .` supports explicit registration. `open --json` returns the URL;
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
