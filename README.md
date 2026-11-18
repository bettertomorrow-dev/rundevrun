# rundevrun

Run development services across projects and existing Git worktrees.
Development software for Linux and macOS; requires Go 1.27 or newer and Git.

```sh
go build -trimpath -o /tmp/rdr ./cmd/rdr
/tmp/rdr --help
```

Create `dev.toml` in your project:

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
rdr config check --json
rdr up api --dry-run --json
rdr up api --json
rdr logs api --tail 50
rdr down api --json
```

`up` registers the project automatically. Discovery and dry-runs do not execute
project commands. Use IDs from `rdr projects --json` and `rdr worktrees --json`
when names are ambiguous.

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
