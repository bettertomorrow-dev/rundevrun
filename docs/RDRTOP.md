# rundevtop

Terminal view of rundevrun services. Quitting leaves services running.

Shared and External configurations work the same way. Run `rundevrun register .` or
`rundevrun up` in an [onboarded project](ONBOARDING.md), then open rundevtop from the repo.

```sh
go build -trimpath -o /tmp/rundevtop ./cmd/rundevtop
/tmp/rundevtop
```

Services are grouped by project and labelled with their worktree. State and logs
refresh once per second. The initial tab follows the latest launch result.

| Key | Action |
| --- | --- |
| Tab | Switch between running services and unresolved launch errors. |
| a | Show all projects when started inside a project. |
| Arrows / j / k | Select a service or scroll logs. |
| PgUp / PgDown | Scroll logs. |
| l / o / s | View logs / open URL / stop service. |
| q | Quit. |

A successful launch clears that service's launch error. Services whose worktree
was deleted remain stoppable through "Stop process without worktree". This closes
publications and cleans tracked processes, skipping unavailable stop hooks.

An app can print `app: url=http://127.0.0.1:20002/s/example/` on its own line.
The latest valid HTTP or HTTPS announcement in the current run overrides the
generated URL. URLs containing credentials are ignored.

See [networking setup](NETWORKING.md) for the Networking view and
[verification](../examples/mini/README.md#verification) for the PTY smoke test.
