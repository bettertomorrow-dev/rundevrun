# Installation

## Release binaries

Release archives contain `rundevrun` and `rundevtop` for macOS and Linux,
for amd64 and arm64. WSL 2 uses the matching Linux archive. Run commands and
keep projects inside the WSL distribution. Native Windows and WSL 1 are not
supported. WSL CI checks service lifecycle and the terminal monitor inside Linux;
Windows browser forwarding and private Tailscale/Cloudflare configurations still
need acceptance on the user's machine.

Git is required for worktree discovery. No Go installation is needed for release
binaries. After the first release is published:

```sh
curl -fsSL https://raw.githubusercontent.com/bettertomorrow-dev/rundevrun/main/install.sh | bash
```

The installer checks SHA-256 checksums and runs version/help checks on both
binaries before replacing installed files. It defaults to
`~/.local/opt/rundevrun` and creates links in `~/.local/bin`. Existing commands
from other installations cause an error rather than being overwritten. It does
not edit shell startup files. Add `~/.local/bin` to PATH when needed.

To select a version or a different installation directory:

```sh
curl -fsSL https://raw.githubusercontent.com/bettertomorrow-dev/rundevrun/main/install.sh -o /tmp/install-rundevrun.sh
RUNDEVRUN_VERSION=v0.1.0 bash /tmp/install-rundevrun.sh
RUNDEVRUN_INSTALL_DIR="$HOME/tools/rundevrun" bash /tmp/install-rundevrun.sh
```

A custom directory receives both binaries directly, without creating PATH links.
Rerun the installer to update. Previous binaries are saved as
`rundevrun.previous` and `rundevtop.previous`. Restore the CLI with
`./scripts/rdr-local rollback` from a checkout, using `RDR_INSTALL_DIR` for a
custom directory, or reinstall a specific release. Neither operation removes
runtime state or registrations. Supervisors retain their running code until you
stop and start affected services with the same selectors using `rundevrun down`
and `rundevrun up`.

On macOS, once Homebrew publication is configured:

```sh
brew install --cask bettertomorrow-dev/tap/rundevrun
brew upgrade --cask bettertomorrow-dev/tap/rundevrun
```

Use one installation method per machine to avoid competing commands on PATH.
Homebrew manages its own installation location and upgrades.

## Build from source

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

Runtime state uses `$RDR_HOME`, then `$XDG_CONFIG_HOME/rundevrun/state`, then
`~/.config/rundevrun/state`. Keep application data elsewhere; retention removes
old run artifacts. Do not commit runtime files or publish their logs.

Run `rundevrun config state` to print the absolute state directory, or add
`--json` and read `results[0].state_dir`. This command works outside repositories,
returns paths that do not exist yet, and does not read or create state.
`RDR_HOME` overrides runtime storage only; configuration stays in the config directory.

Earlier versions stored state under `$XDG_STATE_HOME/rundevrun`, falling back to
`~/.local/state/rundevrun`. If that directory contains files, both commands refuse
to use the new default. Set `RDR_HOME` to the old absolute path to keep using it.
To reset, use that override to stop services with `rundevrun down --all --json`
and the proxy with `rundevrun proxy stop --json` first. Then remove the old runtime
directory and unset `RDR_HOME`. Resetting discards registrations and run history;
`register` or `up` registers projects again. Do not move live state or rewrite
saved manifests, which contain absolute paths and integrity digests.
`XDG_STATE_HOME` now serves only to detect legacy state.

Machine networking settings use `$XDG_CONFIG_HOME/rundevrun`, falling back to
`~/.config/rundevrun`. See [networking](NETWORKING.md) and [existing TLS](LOCAL_HTTPS.md).

Share creation returns its password once, including in JSON. Treat that output
as secret. Repeated shares and status do not return it; `--recreate` rotates the
URL and password. Quick Tunnels do not support SSE.
