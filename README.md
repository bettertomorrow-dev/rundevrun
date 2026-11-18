# rundevrun

Run development services across projects and existing Git worktrees.
Development software for macOS, Linux, and WSL 2. Git is required at runtime.
Building from source requires Go 1.27 or newer.

Install both commands from a published release:

```sh
curl -fsSL https://raw.githubusercontent.com/bettertomorrow-dev/rundevrun/main/install.sh | bash
```

Or, on macOS, after Homebrew publication is configured:

```sh
brew install --cask bettertomorrow-dev/tap/rundevrun
```

See [installation](docs/INSTALL.md) for version pinning, source builds, and
updates. See [CI and releases](docs/CI.md) for checks and the release procedure.

The CLI is `rundevrun`; the terminal monitor is `rundevtop`. For shorter commands,
add these optional aliases to `~/.zshrc` or `~/.bashrc`:

```sh
alias rdr='rundevrun'
alias rdrtop='rundevtop'
```

Use the full executable names in scripts and agent integrations. Shell aliases
usually apply only in interactive shells, and another tool may already use `rdr`.

Create `rundevrun.toml` in your project:

```toml
version = 1
[project]
name = "example"
[services.api]
command = "python3 -m http.server {port} --bind {host}"
port = "auto"
health = { http = "/" }
```

```sh
rundevrun config check --json
rundevrun up api --dry-run --json
rundevrun up api --json
rundevrun logs api --tail 50
rundevrun down api --json
```

`up` registers the project automatically. Discovery and dry-runs do not execute
project commands. Use IDs from `rundevrun projects --json` and `rundevrun worktrees --json`
when names are ambiguous.

For repos where runner files must stay outside the checkout, import a personal
config with `rundevrun config external --file /path/to/rundevrun.toml`. Then run `rundevrun` and
`rundevtop` from the repo as usual. See [Shared and External setup](docs/ONBOARDING.md)
and the [onboarding skill](skills/rdr-onboard/SKILL.md).

- [Installation and state](docs/INSTALL.md)
- [Terminal interface](docs/RDRTOP.md)
- [Local and tailnet networking](docs/NETWORKING.md)
- [Existing HTTPS infrastructure](docs/LOCAL_HTTPS.md)
- [Configuration and contributor reference](docs/TECH_SPEC.md)
- [Examples and verification](examples/mini/README.md)

## Checks

```sh
go vet ./...
go test ./...
go test -race ./...
node --test scripts/domain-wizard-token.test.mjs
```
