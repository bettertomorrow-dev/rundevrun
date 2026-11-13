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

Use `direct_url` for service-to-service requests. `url` is the address intended for a browser. While the local reverse proxy is not implemented, `url` equals `direct_url`.

## Public CLI scenario

Run `python3 scenario.py /path/to/rdr`, or set `RDR_BIN` to the installed binary. The script creates a disposable Git repository with four worktrees, registers it, discovers roots and IDs through JSON output, validates each config, and drives the public CLI. It never reads runner state or private manifests. It attempts `down --all` and targeted public `down` calls before deleting its temporary directory, including on errors.

The fixture and config checks work before graph support. The scenario reports `INCOMPLETE` when stage 3 commands are missing or return an error. It prints `PASS` only after the complete dependency and cascade sequence and a clean public `status` check. If cleanup cannot confirm that every instance is stopped with no pending preparation, it keeps both the fixture and its `RDR_HOME`, prints their path, and exits nonzero. Git and CLI subprocesses have 30-second timeouts; fixture Git runs with user Git environment selectors cleared and hooks, fsmonitor and commit signing disabled.
