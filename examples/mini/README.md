# Mini multi-worktree example

This synthetic project has three services. `catalog` and `reports` serve HTTP. `reports` fetches the selected catalog identity on `/identity`. `indexer` has no port and polls its selected catalog every two seconds. Python 3 standard library is enough.

The intended graph uses one `catalog` from `main` for `reports` in `consumer-a`, `reports` in `consumer-b`, and `indexer` in `consumer-a`. A fourth checkout has an independent catalog. Rebinding only `reports` in A to that third checkout, then restarting it, should make its response identify the third checkout. The indexer and B's report continue using the original catalog.

## Manual commands

Create three Git worktrees yourself. `rdr` discovers them but never creates or removes them.

```sh
rdr register /path/to/mini
rdr projects --json
rdr worktrees --project mini --json
rdr services --project mini --worktree /path/to/mini --json
rdr config check --project mini --worktree /path/to/mini --json

rdr bind reports catalog --project mini --worktree /path/to/consumer-a --to-worktree /path/to/mini --json
rdr bind indexer catalog --project mini --worktree /path/to/consumer-a --to-worktree /path/to/mini --json
rdr bind reports catalog --project mini --worktree /path/to/consumer-b --to-worktree /path/to/mini --json
rdr up catalog --project mini --worktree /path/to/mini --json
rdr up reports indexer --project mini --worktree /path/to/consumer-a --json
rdr up reports --project mini --worktree /path/to/consumer-b --json
rdr status --project mini --json
rdr logs indexer --project mini --worktree /path/to/consumer-a --tail 20

rdr up catalog --project mini --worktree /path/to/catalog-c --json
rdr bind reports catalog --project mini --worktree /path/to/consumer-a --to-worktree /path/to/catalog-c --json
rdr restart reports --project mini --worktree /path/to/consumer-a --json
rdr logs reports --project mini --worktree /path/to/consumer-a --tail 20
rdr down reports --project mini --worktree /path/to/consumer-b --json
rdr down catalog --project mini --worktree /path/to/mini --json
rdr down catalog --cascade --project mini --worktree /path/to/mini --json
```

The first `down catalog` should refuse while the indexer still consumes it. `--cascade` explicitly stops that consumer first. Stopping a consumer on its own leaves its catalog running.

Use `direct_url` for service-to-service requests. `url` is the address intended for a browser; when routing is disabled, it falls back to `direct_url`.

## Public CLI scenario

Run `python3 scenario.py /path/to/rdr`, or set `RDR_BIN` to the installed binary. The script creates a disposable Git repository with four worktrees, registers it, discovers roots and IDs through JSON output, validates each config, and drives the public CLI. It never reads runner state or private manifests. It attempts `down --all` and targeted public `down` calls before deleting its temporary directory, including on errors.

The fixture and config checks work before graph support. The scenario reports `INCOMPLETE` when stage 3 commands are missing or return an error. It prints `PASS` only after the complete dependency and cascade sequence and a clean public `status` check. If cleanup cannot confirm that every instance is stopped with no pending preparation, it keeps both the fixture and its `RDR_HOME`, prints their path, and exits nonzero. Git and CLI subprocesses have 30-second timeouts; fixture Git runs with user Git environment selectors cleared and hooks, fsmonitor and commit signing disabled.

## Public switch scenario

Run `python3 switch_scenario.py /path/to/rdr` or set `RDR_BIN`. This separate script checks single-service, service-list, mixed-target file, and `--all-services` switches. It also checks that an unknown final selector or invalid final file mapping leaves public run IDs unchanged, and that a target build failure preserves the source catalog run. Cleanup uses `down --all` only inside the script's isolated `RDR_HOME`; it prints `PASS` only after public status confirms every instance stopped with no unknown state or pending preparation. On cleanup failure, it retains and prints the fixture and runtime paths.

## Public TLS and proxy scenario

Run `python3 tls_scenario.py /path/to/rdr` (or set `RDR_BIN`) on a machine with `openssl`. It creates a disposable Git project, two worktrees, isolated state and a short lived test CA. The client trusts only that CA and dials the local HTTPS listener while sending each route hostname as SNI; it does not edit DNS or system trust. The scenario checks HTTPS dependency routing, a Secure cookie roundtrip, streamed SSE, a WebSocket/HMR frame, alias collision behavior, stopped-route isolation, and proxy restart/reload behavior with unchanged app run IDs. An invalid certificate reload must leave the existing proxy and apps available. It uses public JSON CLI state and retains the fixture/runtime if cleanup cannot confirm every instance stopped.

## Public offline share scenario

Run `python3 share_scenario.py /path/to/rdr` (or set `RDR_BIN`) on a machine with `openssl`. This uses a temporary executable named `cloudflared` that opens only an ephemeral loopback TLS listener and forwards to the publication's loopback gateway. Its provider map contains only its own hostname and port; the client uses that map to dial loopback while validating a temporary wildcard certificate and SNI. No public tunnel or DNS change is made. The scenario checks live and configured-default snapshot shares, one-time password output, protected API/dependency/WebSocket requests, app-cookie roundtrip, immutable snapshot bytes, failed and successful recreation, unshare, and closing a share before restarting a routed dependency. Quick Tunnel SSE support is not tested. `PASS` requires public status to confirm apps stopped and no publications remain; failed cleanup retains the fixture and runtime.

The provider map lives inside the disposable test directory and contains only the fake hostname and its allocated loopback port. Both fixture scripts use bounded subprocess and socket timeouts. The share scenario checks applications and publications through public JSON; it never reads runner records or kills a process by PID.

## Real Cloudflare acceptance scenario

To test real outbound network access and publish only synthetic password-protected data, run `python3 cloudflare_scenario.py /path/to/rdr`. It uses `cloudflared` from `PATH`, or the executable named by `CLOUDFLARED_BIN`, with an isolated `RDR_HOME` and config directories. It preserves `HOME` and existing tunnel processes. It creates only a synthetic Git project, uses system TLS trust and normal DNS for the real HTTPS URL, then checks live and snapshot authentication, Secure cookies, a WebSocket frame, snapshot immutability, unshare, and public cleanup. It does not test SSE, use Cloudflare credentials, or read private runner state. If cleanup cannot confirm stopped apps and no publications, the script retains the fixture and runtime.
