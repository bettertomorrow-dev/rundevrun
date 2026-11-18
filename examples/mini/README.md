# Mini multi-worktree example

Three Python standard-library services demonstrate dependency bindings:
`catalog` and `reports` serve HTTP; `reports` returns its selected catalog's
identity at `/identity`. The portless `indexer` polls its selected catalog.
See [dev.toml](dev.toml).

## Worktree bindings

Create Git worktrees yourself; rdr only discovers them. From the mini project:

```sh
rdr register . --json
rdr worktrees --project mini --json
rdr bind reports catalog --worktree /path/to/consumer --to-worktree /path/to/catalog --json
rdr up reports --worktree /path/to/consumer --json
rdr logs reports --worktree /path/to/consumer --tail 20
rdr down reports --worktree /path/to/consumer --json
```

Rebind and restart `reports` to change its catalog without changing other
consumers. `down catalog` refuses while live consumers remain; `--cascade` stops
them first. Stopping a consumer leaves its dependencies running. Use `direct_url`
for service-to-service requests and `url` for browser access.

## Verification

Build from the repository root. Scenarios require Python 3 and Git; TLS and
offline sharing also require OpenSSL.

```sh
go build -trimpath -o /tmp/rdr ./cmd/rdr
python3 examples/mini/scenario.py /tmp/rdr
python3 examples/mini/switch_scenario.py /tmp/rdr
python3 examples/mini/tls_scenario.py /tmp/rdr
python3 examples/mini/share_scenario.py /tmp/rdr
python3 scripts/rdrtop_smoke.py
```

| Scenario | Coverage |
| --- | --- |
| `scenario.py` | Discovery, cross-worktree bindings, dependency startup, cascade shutdown. |
| `switch_scenario.py` | Single/batch switches, invalid selectors, build failure preserving the source. |
| `tls_scenario.py` | Temporary CA, Secure cookies, SSE, WebSocket, aliases, proxy restart and invalid reload. |
| `share_scenario.py` | Fake loopback tunnel, password gate, API/WebSocket, immutable snapshots, recreation and unshare. |
| `scripts/rdrtop_smoke.py` | PTY interaction, logs, terminal restoration, service survival and stop hooks. |

Fixtures use disposable projects and isolated runtime state. They confirm cleanup
through public CLI status before deleting files; uncertain cleanup retains the
fixture and reports its path. TLS trusts a temporary CA only in the test client
and changes neither DNS nor system trust. Offline sharing creates no public tunnel.

For real outbound HTTPS, run `python3 examples/mini/cloudflare_scenario.py /tmp/rdr`.
It requires `cloudflared` on PATH or `CLOUDFLARED_BIN` and publishes synthetic
password-protected data. It checks live/snapshot authentication, cookies,
WebSocket, immutability, and unshare through normal DNS and system TLS trust.
Quick Tunnels do not support SSE.

Run platform-specific lifecycle and listener tests on each target OS; cross-builds
are insufficient. Synthetic TLS does not verify your browser trust or ingress,
and tailnet setup status does not prove access from a second device.
