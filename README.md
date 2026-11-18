# rundevrun

CLI for development services across projects and existing Git worktrees.
This is development software, not a stable release.

Requires Go 1.27 or newer and Git for worktree discovery. See
[local installation and usage](docs/INSTALL.md) for build and setup instructions.

```sh
go build -trimpath -o /tmp/rdr ./cmd/rdr
/tmp/rdr --help
```

The separate `rdrtop` terminal app visualizes rdr-managed services:

```sh
go build -trimpath -o /tmp/rdrtop ./cmd/rdrtop
/tmp/rdrtop
```

See [rdrtop usage](docs/RDRTOP.md).

See [local and tailnet networking](docs/NETWORKING.md) for configuration,
provider setup, credential handling, recovery, and migration.

For guided setup with your own domain on macOS or Linux, use the explicit-only
[`rundevrun-domain-setup-wizard`](skills/rundevrun-domain-setup-wizard/SKILL.md)
agent skill. It checks prerequisites, guides Cloudflare onboarding, and previews
the private Tailscale/DNS/certificate changes before applying them.

Project configuration lives in `dev.toml`:

```toml
version = 1
[project]
name = "example"
[services.api]
command = "python3 -m http.server {port} --bind {host}"
port = "auto"
health = { http = "/" }
```

The discovery commands validate configuration without running project commands:

```sh
rdr config check --json
rdr register . --dry-run --json
rdr register . --json
rdr projects --json
rdr worktrees --project example --json
rdr services --project example --json
rdr status --project example --json
rdr up api --dry-run --json
rdr up api --json
rdr logs api --tail 50
rdr open api --json
rdr doctor --json
rdr proxy status --json
rdr restart api --json
rdr down api --json
```

Registration records configured services as stopped. Independent clones receive
different IDs even when names match. Select ambiguous projects or worktrees by
the IDs in JSON output. Discovery and status do not create runtime state;
registration stores private state under `$RDR_HOME`, `$XDG_STATE_HOME/rundevrun`,
or `~/.local/state/rundevrun`.

For reproducible checks and known platform limits, see
[verification](docs/ACCEPTANCE.md). See [local HTTPS setup](docs/LOCAL_HTTPS.md)
for existing TLS infrastructure and [technical contracts](docs/TECH_SPEC.md)
for contributor details.

Checks:

```sh
go vet ./...
go test ./...
go test -race ./...
```
