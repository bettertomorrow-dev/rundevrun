# rundevrun

CLI for development services across projects and existing Git worktrees.
Implementation follows [the plan](docs/IMPLEMENTATION_PLAN.md) and
[PRD](docs/PRD.md). This is an intermediate implementation, not a release.

Requires Go 1.27 and Git for Git worktree discovery.

```sh
go build -o /tmp/rdr ./cmd/rdr
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
```

Registration records configured services as stopped. Independent clones receive
different IDs even when names match. Select ambiguous projects or worktrees by
the IDs in JSON output. Discovery and status do not create runtime state;
registration stores private state under `$RDR_HOME`, `$XDG_STATE_HOME/rundevrun`,
or `~/.local/state/rundevrun`.

Lifecycle, hooks, switching, proxy and publication remain in progress. The first
release requires migration of MillDrum and acceptance on twikko, Linux and macOS.
See [technical contracts](docs/TECH_SPEC.md) and [review status](docs/REVIEWS.md).

Checks:

```sh
go vet ./...
go test ./...
go test -race ./...
```
