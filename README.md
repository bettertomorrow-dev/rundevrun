# rundevrun

CLI for development services across projects and existing Git worktrees.
Implementation follows [the plan](docs/IMPLEMENTATION_PLAN.md) and
[PRD](docs/PRD.md). This is an intermediate implementation, not a release.

Requires Go 1.27 or newer and Git for worktree discovery. Go 1.27.4 was used
for the current local build. See [local installation and usage](docs/INSTALL.md)
for a repeatable build, private install, and JSON-first workflow.

```sh
go build -trimpath -o /tmp/rdr ./cmd/rdr
/tmp/rdr --help
```

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

Graph lifecycle, hooks, batch switch, live sharing, and static snapshots passed
Linux tests and public CLI scenarios. Unknown process ownership refuses unsafe
cleanup and keeps state available for inspection. MillDrum and twikko ran together
under one binary with disposable data. The MillDrum wrapper, watcher, backup,
seed, sign-in, protected preview and legacy handover passed Linux acceptance.
Real Cloudflare HTTPS, password authentication, WebSocket and immutable snapshots
also passed. Darwin arm64/amd64 cross-builds passed; actual macOS process checks
remain open because no Mac is available. The user's HTTPS ingress for
`home.litvinovich.dev` also remains unprepared. These two conditions keep final
acceptance open.
See [local HTTPS setup](docs/LOCAL_HTTPS.md), [acceptance evidence](docs/ACCEPTANCE.md),
[technical contracts](docs/TECH_SPEC.md) and [review status](docs/REVIEWS.md).

Checks:

```sh
go vet ./...
go test ./...
go test -race ./...
```
