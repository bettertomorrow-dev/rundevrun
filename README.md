# rundevrun

rundevrun runs local dev services from the Git worktree you want to test.
Reviewing a PR? Open an agent chat in its worktree and type `rundevrun`.
An agent with the rundevrun skill reads the project config, starts the services
you need, and sends you the links. If you're working on your own, you can start
the same services from the terminal and see what's running in each worktree
with `rundevtop`.

---

Set up your existing launch commands once in `rundevrun.toml`.
Testing two PRs at once? rundevrun can assign free ports so both can run
side by side.
When you're done, stop that worktree's services with one command.

rundevrun runs on macOS, Linux, and WSL 2. Git is required at runtime.
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

## Use rundevrun with an agent

### Onboard a project

Run `rundevrun guide` to list the available guides.
Ask an agent to run `rundevrun guide onboard` from the project checkout and follow the
returned instructions. No skill installation is required. Choose Shared mode for
a `rundevrun.toml` the team can commit, or External mode for personal config.

> Onboard this repo with rundevrun in External mode.

The agent validates the config, previews the launch, and registers the project.
Include a start request if you want it to launch services. See
[project setup](docs/ONBOARDING.md) for guide updates and local overrides.

### Install the optional service skill

The [rundevrun skill](skills/rundevrun/SKILL.md) helps agents operate configured
services. Install the CLI first, then copy the skill from this checkout:

```sh
skills_dir="${CODEX_HOME:-$HOME/.codex}/skills"
mkdir -p "$skills_dir"
cp -R skills/rundevrun "$skills_dir/"
```

Start a new session to use it. Other agents that support `SKILL.md` can use
their personal skills directory as the destination.

### Run services

Ask the agent to use the [rundevrun skill](skills/rundevrun/SKILL.md) once the
project has a config. Name the services and the branch or worktree when relevant.

> Use the rundevrun skill to start the API in the `feature/login` worktree and report its URL.

The agent checks the target worktree, runs the requested operation, and verifies
the result. Use the same skill to read logs, stop services, or move them between
worktrees.

## Configure a project by hand

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
and [agent-assisted onboarding](docs/ONBOARDING.md#agent-assisted-onboarding).

Install the [rundevrun skill](skills/rundevrun/SKILL.md) in your agent's personal
skills directory to manage services through the CLI.

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
