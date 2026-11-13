# rundevrun technical contracts, schema 1

Implementation baseline: PRD 0.4, runner initial commit `ef017bc`.
MillDrum inspected at `1184e35aba0a8c09699bff66439d00ba269b8209`.
This document fixes contracts before builders start. Changes need Sol integration
before dependent tasks resume. A release requires all stages of IMPLEMENTATION_PLAN.

## Toolchain and boundaries

Go 1.27, module `github.com/bettertomorrow-dev/rundevrun`, binary `cmd/rdr`.
Development toolchain on this host: Go 1.27.4 in
`/home/alex/.cache/rundevrun-toolchain/go/bin`. Use PATH explicitly for checks.
No machine path is compiled into the binary. Linux and macOS are supported.
TOML decoding may use `github.com/pelletier/go-toml/v2`; process inspection may use
`golang.org/x/sys/unix`. Pin versions when first used. All other work starts with
the standard library. No dependency on any MillDrum package.

## Identity, discovery and selectors

Canonical paths are absolute, cleaned and symlink-resolved existing paths.
Git project identity is SHA-256 of the canonical absolute `git rev-parse
--git-common-dir` path. A non-Git project uses its canonical registered root.
Prefix IDs with `p_` and `w_`, using the full hex SHA-256; worktree ID hashes its
canonical root. Instance ID is `<project-id>/<worktree-id>/<service>`.
Independent clones never merge by remote URL or name. Project names are labels,
not keys. A duplicate name is allowed but requires an ID selector.

Find the nearest ancestor dev.toml without executing project commands. Git reads
use argument arrays, never a shell; worktree listing uses `git worktree list
--porcelain -z`. Git common directory and worktree paths resolve relative to their
command working directory. Detached HEAD is valid and has no branch selector.
Non-Git projects expose just the registered root. Linked worktrees share project
identity, but each loads its own dev.toml and must declare the same project name.
Missing configuration in another worktree is reported, not silently substituted.

Resolution order: exact ID, canonical path, unique directory/branch label.
Ambiguity returns candidates with IDs and paths. A branch is only a selector for
an existing checkout. Never checkout, create, remove, or fetch a worktree.
Outside a registered root, mutations require project and worktree selectors or a
file containing full references. Default down selects configured/current known
instances in the current worktree; global down requires --all.

Desired dependency binding is persisted by consumer instance ID and dependency
service name, with an exact target instance ID in the same project. Default is
the dependency in the consumer worktree. Only declared dependencies can be bound.
Live runs retain effective bindings separately; bind reports restart_required and
does not change running environments. Missing/stopped chosen dependencies never
fall back to another worktree. Discovery includes configured stopped services.

## dev.toml schema

Unknown fields at every depth and unknown versions are errors with filename and
field path. Service/project names match `[a-z][a-z0-9_-]*`. Empty services fail.
Fields below are the complete schema; omitted optional tables are disabled.

| Field | Type and requirement | Default and constraints |
| --- | --- | --- |
| version | required integer | Exactly 1. |
| project | required table | Only required nonempty name. |
| services | required table of service tables | At least one service. |
| routing | optional table | Only domain, a nonempty DNS domain override. Machine listener/key settings forbidden here. |
| service.command | required nonempty string | Whitespace-only rejected. |
| service.cwd | optional nonempty string | '.', confined to existing worktree directory. |
| service.build | optional nonempty string | No build if absent. |
| service.protocol, port | optional string, integer or 'auto' | Rules below. |
| service.depends_on | optional string array | [], unique existing service names. |
| service.env | optional string map | {}, valid shell variable names, no RDR_* or __RDR_* keys. |
| service.build_timeout/start_timeout/stop_timeout/hook_timeout | optional duration strings | 5m/30s/20s/60s, positive. |
| service.health | optional table | Exactly one method, optional timeout defaulting to start_timeout, never greater. |
| service.hooks | optional table | Only four hook names below, nonempty command values. |
| service.routes | optional table array | [], required path and dependency strings, optional boolean strip_prefix=false. |
| service.share | optional table | HTTP only; required mode=live or snapshot; auth defaults to password, no other auth accepted. |
| share.build, share.dir | optional nonempty strings | Both required for snapshot and forbidden for live. dir relative to cwd. |
| share.env | optional string map | {}, snapshot-only, same reserved-name restrictions as service.env. |

All tables reject unknown keys. Service fields omit `services.<name>.` here.
Absent share permits explicit live sharing of HTTP; snapshot requires configured
build/dir. --mode live may override configured snapshot without using snapshot
build/env/dir; --mode snapshot without snapshot config fails. Empty share, health
or routing tables are invalid. Empty optional command strings are invalid, not
omission. Empty hooks/env/routes are valid. Snapshot dir may not exist at config
load: validate lexical confinement and closest existing ancestor now, revalidate
full canonical confinement after build. cwd must exist at load.

```toml
version = 1
[project]
name = "example"

[services.api]
cwd = "."
build = "go build -o {run_dir}/api ./cmd/api"
command = "{run_dir}/api --addr 127.0.0.1:{port}"
protocol = "http"
port = "auto"
depends_on = []
build_timeout = "5m"
start_timeout = "30s"
stop_timeout = "20s"
health = { http = "/health", timeout = "30s" }
env = { HOST = "{host}", PORT = "{port}" }
hooks = { before_stop = "./scripts/drain", after_stop = "./scripts/backup" }
hook_timeout = "60s"

[services.console]
cwd = "apps/console"
command = "pnpm dev -- --host 127.0.0.1 --port {port} --strictPort"
protocol = "http"
port = "auto"
depends_on = ["api"]
env = { VITE_API_URL = "{dependencies.api.url}" }
routes = [{ path = "/api", dependency = "api", strip_prefix = false }]
share = { mode = "snapshot", auth = "password", build = "pnpm build", dir = "dist", env = { VITE_API_URL = "/api" } }

[services.worker]
command = "./scripts/worker"
depends_on = ["api"]
env = { API_URL = "{dependencies.api.direct_url}" }
```

Default cwd is '.', default protocol is 'process' without a port and 'http' with
a port. Explicit protocols: process, tcp, http. process forbids port; tcp/http
require one. Port is integer 1..65535 or string 'auto'; omission means no port.
health has exactly one of http relative absolute-path string, tcp=true, or
process=true, plus optional positive duration timeout. No health defaults to
process liveness, reported running rather than ready. This satisfies dependency
startup without claiming application health. Explicit health.process=true uses
the same gate and reports running. HTTP without a healthcheck becomes routable
once liveness and owned loopback listener are verified, still reported running.
HTTP checks require http
protocol and accept status 200..299 without following redirects. TCP checks
require a port. Default timeouts are those above. All durations must be positive.
Hooks allow before_start, after_start, before_stop, after_stop strings only.
Dependencies must exist in the configuration and be unique; static cycles fail.
Dependency names normalized to uppercase with nonalphanumerics replaced by '_'
must be unique. routes require HTTP and a declared HTTP dependency. Prefix paths
start with '/', reject query/fragment and duplicate prefixes; longest path match
on a segment boundary wins. No arbitrary upstream URL is accepted.

cwd must remain inside the canonical worktree after symlink resolution. Snapshot
dir resolves relative to cwd and must remain there after symlink resolution.
Build outputs belong in run_dir; project builds that write source directories
must publish by copying into a fresh snapshot directory, never serve source dist.
Configuration digest is SHA-256 of raw dev.toml bytes; live digest is immutable.

## Commands, substitutions and environment

Command strings run as `/bin/sh -c` in cwd. This is trusted project code executed
only for explicit lifecycle/share operations. Never execute a command to validate
configuration, register, list, status or dry-run. Do not interpolate arbitrary values into shell source. Each placeholder becomes
`"${__RDR_SUB_0}"`, incrementing its index, with raw value passed in environment.
The entire __RDR_ prefix is reserved and stripped from inherited env. Thus
`{run_dir}/api` becomes `"${__RDR_SUB_0}"/api`, preserving one word with spaces,
quotes, dollars or newlines in the value. No recursive substitution.

Commands containing runner placeholders use a restricted grammar: balanced shell
single/double quotes may enclose ordinary text, but placeholders must be outside
quotes. Reject unquoted backslash or '#', backticks, '$(' and '<<' outside single
quotes, including inside double quotes. This excludes comments, command/arithmetic
substitution, heredocs and ambiguous escapes. Reject a recognized placeholder in
quotes. `$VAR` and `${VAR}` remain shell syntax, the latter consumed whole before
brace scanning and allowing only plain variable names, no parameter operators.
Literal {{ / }} yields a single brace; unmatched braces/unknown names fail.
Commands with no runner placeholders may use ordinary trusted shell syntax.
Env strings expand raw values without shell evaluation, with literal-brace
escapes. Validate placeholders and syntax without executing shell.

| Placeholder or example | Value or handling |
| --- | --- |
| host / port | 127.0.0.1 / decimal assigned port; latter absent without port. |
| direct_url / url | Direct HTTP / named HTTPS or direct HTTP; absent for non-HTTP. |
| run_dir | Absolute private directory of specific prepared or active run. |
| worktree / project / service | Canonical worktree path / configured project name / service name. |
| dependencies.<name>.host/port/direct_url/url | Exact selected declared dependency, same availability rules. |
| `{run_dir}/api --port {port}` | Accepted, safe quoted env expansions. |
| `PORT={port} ./app; echo ${HOME}` | Accepted, shell variable unchanged. |
| `echo '{port}'` or `echo "{port}"` | Rejected, quoted placeholder. |
| `echo $(cat file) {port}`, heredoc plus `{port}`, comment plus `{port}` | Rejected. |
| `echo {{literal}} {port}` | Accepted, literal braces and one expansion. |

Unknown/unavailable substitutions fail before any build.

Environment order: inherited env excluding RDR_*, configured service env,
then computed RDR_* and internal substitution variables. Config cannot assign
RDR_* or internal substitution names. No automatic PORT/API_URL mapping.
RDR_HOST=127.0.0.1; RDR_PORT only for port services; RDR_DIRECT_URL only HTTP;
RDR_URL is routed HTTPS URL or direct HTTP URL when routing is disabled.
RDR_DEP_<NORMALIZED_NAME>_* follows the same availability rules.
Build and start receive the same new planned addresses. Stop-hooks use the
stopping run's immutable stop contract: saved commands, cwd, run_dir, effective
bindings and fully resolved environment captured when that run started, even if
dev.toml or desired binds changed. Manifest privately stores this contract and
never exposes environment in status. Missing old cwd/script fails explicitly;
never use new version's stop contract. Start-hooks use new prepared values.
Scripts remain project-owned and may change on disk: runner saves invocation,
not a copy of all source. Hooks additionally
receive RDR_REASON, RDR_INSTANCE_ID, RDR_OLD_WORKTREE, RDR_NEW_WORKTREE and
RDR_OPERATION_ID. Snapshot env overrides service env and must explicitly provide
publication-safe values for dependency URLs; reject local dependency URLs in
snapshot substitutions. Private credentials are never included in JSON status.
No port changes after preparation. Cancellation and timeout terminate command
groups and wait for their descendants before returning.

## State, locking and process ownership

Runtime root: RDR_HOME override, otherwise XDG_STATE_HOME/rundevrun, otherwise
~/.local/state/rundevrun on both OSes. Private dirs 0700, files 0600, including
existing paths, reject symlink state files. Never store application data here.
registry.json schema=1 contains revision counter and projects keyed by ID, with
instances, desired bindings, reservations and intents nested per project. Decode
top-level schema/revision/projects strictly but retain each project as raw JSON
and validate it independently. Syntactically corrupt registry is a global
STATE_CORRUPT error, never overwritten. Semantically invalid project record is a
per-project listing error; healthy reads/updates continue, preserving the invalid
raw value. Corrupt manifest/supervisor state affects that run and is never treated
as stopped. Each run has a random ID and
runs/<id> containing manifest.json, supervisor.json and logs. Supervisor alone
writes supervisor.json. CLI alone updates registry under its lock.

Persist JSON by unique same-directory temporary file, chmod, write, fsync,
close, rename and parent directory fsync. Decode errors fail closed with path,
never replace corrupt state with empty state. Reads need no long operation lock.
Project/run corruption is isolated as above; global syntax damage is a global error.

Nonblocking flock operation locks per instance, acquired in sorted ID order for
the entire affected graph, including effective consumers and selected targets.
Global registry lock is held only during read-modify-write and port assignment,
never while building, waiting, invoking hooks or calling supervisors. Lock order:
sorted instance locks, then sorted per-run spawn gates, then registry lock;
supervisor never takes instance locks. Spawn gate is per-run flock shared by
supervisor and recovery, held during authorization/spawn/identity persistence,
not readiness or hooks.
After locks, re-resolve graph and revisions, retry planning or report conflict.
Locks release on process exit. Supervisor has its own per-run writer lock.

States are stopped, starting, running, ready, unhealthy, failed. Intent phase is
separate: preparing, prepared, spawning, started, stopping, stopped, hook_failed,
complete, interrupted. Write intent with run ID and manifest BEFORE spawn.
Manifest includes addresses, effective bindings, digest, operation ID and immutable stop contract.
Spawn detached rdr internal supervisor with run ID and root, not shell code in
argv. Supervisor acquires spawn gate, reads authorization under registry lock,
releases registry lock but RETAINS gate, then durably writes its identity and
claim=accepted to supervisor.json. This claim is the authorization linearization
point; recovery cannot revoke while gate is held. Supervisor spawns a child rdr
bootstrap with inherited acknowledgement pipe. Bootstrap runs no project code
until supervisor durably records its kernel identity and sends a one-byte go.
EOF before go exits without exec. On go bootstrap execs /bin/sh with same PID and
start identity. Release gate only after identity is durable and go sent.
Supervisor death before persistence closes pipe and bootstrap exits; after
persistence recovery can find/stop child, even racing exec. Handshake timeout 5s.
The supervisor.json writer lock is separate and short-lived; recovery never
writes supervisor.json. CLI timeout acquires spawn gate while holding operation
locks, then re-reads claim and child identity. Revoke only unclaimed intents;
claimed ones require verified supervisor and child cleanup before clearing.
A paused supervisor holding gate gives BUSY until resumed or explicitly stopped
by verified identity. Successful down requires confirmed cleanup, never merely
revocation. Supervisor survives CLI exit in a separate session, handles TERM and
child exit, and never automatically crash-restarts the service.

Identity requires PID, kernel start identity, PGID, SID and boot identity. Linux
uses /proc start ticks plus boot_id, macOS start sec/usec plus kern.boottime.
Empty/mismatched identity is never owned. Do not kill a PID based on port alone.
Track group members while supervisor lives, and retain verified descendants for
cleanup after leader death. A group can be signalled only with a matching live
member still in the recorded group/session. Re-check before signalling; document
the unavoidable check-to-signal race. Processes that intentionally daemonize out
of the session are unsupported and must not be reported as safely contained.
Readiness requires both live owned process and proof that the selected listener
belongs to its tracked group, not merely a successful HTTP/TCP response. Platform
inspection fails closed if listener ownership cannot be established. Verify bind
address too: reject 0.0.0.0, :: including dual-stack, and all non-loopback addresses
on configured port even with matching owner. Inspect other TCP/UDP listeners of
tracked members too: any non-loopback listener fails startup and cleans run.
Recheck while supervised. Guarantee covers tracked processes, not an unrelated
daemon they contact or an intentionally escaped child. Daemonizing out of the
session is unsupported. Test wildcard and dual-stack separately on both OSes.

Recovery joins registry intent and per-run state, verifies OS identity and marks
stale runs stopped/failed without signalling foreigners. Status is read-only;
it may compute stale/in-progress warnings but never revoke intents, release
reservations or run hooks. Mutating recovery obtains all affected instance locks,
then spawn gates, re-reads under registry lock. Busy locks mean BUSY, never
abandonment, even during a long build. Active owners stay untouched. Recovery
writes registry results only, not supervisor files. Uncertain live ownership
is an explicit error. No generic data rollback or replay of interrupted hooks.
After interrupted switch, expose step journal and require a new explicit command.
Do not silently re-run a migration or after_stop hook.

Port assignment occurs for the entire graph before any build, under registry
lock, excluding all live/reserved ports across projects and checking loopback
availability. Bind sockets may be released before external programs start;
registry reservation protects only against rdr, not arbitrary programs. On bind
conflict fail, clean owned new processes and report conflict. Preparation failure
preserves old processes. A conflict after confirmed stop leaves old runs stopped:
retain artifacts/history, never promise automatic restoration.
Restart preserves its port when possible; switch targets have their own address.

Logs per run/stage: build, hook name, application, supervisor. Rotate at 10 MiB,
keep 3 segments per stream and 5 completed runs per instance; never remove active
artifacts or app data. Log write failure fails operation or marks runtime failed
and cleans owned child processes. Status points to logs but excludes raw env.

## One operation planner and executor

Planner resolves all selectors/configuration, desired AND effective graph,
cycles, normalized env names, routes and affected consumers before mutation.
Plan contains exact IDs, digests, expected revisions, addresses, preparations,
hooks, stop/start ordering, binding changes and publication closures. Dry-run
uses the same planner but only reads: no mkdir, locks written to disk, port
reservation, alias registration, execution or network tunnel. Available ports
in dry-run are tentative. Execution rebuilds/revalidates plan under locks.

up starts dependencies topologically and waits successful startup gates before
consumers: configured HTTP/TCP health success or verified process liveness, plus
owned loopback listener for every port service;
already-running instances are unchanged even if config changed. Report drift
and restart_required. Partial successful dependencies remain after later failure.
down stops only selected instances, rejects effective live consumers outside the
set unless --cascade, then stops reverse topologically. Consumer stop never adds
its dependencies. restart prepares new build before stopping old run.

Switch file schema, unknown keys rejected:

```json
{"version":1,"mappings":[{"from":{"project":"p_<hash>","worktree":"w_<hash>","service":"api"},"to":{"project":"p_<hash>","worktree":"w_<hash>","service":"api"}}]}
```

Each reference accepts exact IDs or resolvable paths/labels. Project and service
must match within each mapping; different mappings may target different worktrees.
Reject identical from/to, duplicate source, duplicate target, source/target
overlap including chains/cycles, cross-project mappings, missing services, stopped
sources and already-live targets before preparing. --file cannot mix shorthand
selectors. --all-services expands only live services in the explicit source.
Compute transitive affected effective consumers and rewritten target graph,
validate it, allocate all addresses, then build every replacement AND consumer
requiring rebuilt environment before stopping anything. Each instance transitions
once. Close affected shares, stop consumers then sources reverse topologically,
before_stop -> TERM/wait -> after_stop for each real stop. Failure before_stop
keeps that process; after_stop failure prevents its replacement. Start new graph
topologically: before_start -> spawn/readiness -> after_start. Commit desired
bindings as journaled edges when their new dependencies are ready, then start
consumers. A selected consumer starts only its target version. Do not change
bindings of unrelated instances, including explicit target-side overrides.

Switch edges follow these rules before preparation:

| Consumer situation | Result |
| --- | --- |
| Live unselected consumer, desired equals effective | Rewrite mapped effective dependency A to B in desired and restart with B. |
| Any live affected consumer, including selected source, with pending desired/effective difference | Reject BINDING_INVALID before build. Require explicit restart/rebind first. Never discard or implicitly apply pending bind. |
| Stopped unselected consumer | No lifecycle; rewrite existing explicit desired edge A to B. Other desired/default edges unchanged. |
| Selected consumer moving to target | Target explicit binds win. Otherwise inherit source explicit binds translated through mappings. Source defaults translate through mappings if selected; otherwise resolve in target worktree. |
| Target explicit override points to a source that will stop | Reject BINDING_INVALID before build, require explicit target rebind. |

Include stopped edge rewrites in plan. Validate resulting desired and effective
restart graphs for cycles and configured same-project targets. No target override
is silently discarded. Every stop step depends on confirmed death of all
affected live consumers. A before_stop error or surviving child blocks stopping
its dependency and transitive dependencies, marked not_run. An after_stop error
blocks that instance replacement and dependent starts, but confirmed death may
permit safe dependency stops. Start requires confirmed old stop, successful
mandatory hooks and dependency startup gates. Independent branches may finish;
failed consumer stop never permits dependency stop.

after_start failure keeps the live process and blocks dependent steps. On any
failure stop dependent execution, report completed/failed/not_run steps, retain
successful independent work and verified live state. Never automatically restore
old binaries over possibly migrated data. Persist each step boundary and final
actual state. Publications stay closed after switch, with no automatic reopen.

## CLI result contract

Public command grammar follows the PRD examples. --json works before/after
selectors. No prompts. No args prints help. Global flags --project, --worktree,
--json, --dry-run; --all and --all-services have distinct scopes. logs --follow
streams human log text; --json --follow is rejected to retain one-document stdout.
All progress/build output goes to stderr. Exactly one JSON object on stdout:

```json
{"version":1,"command":"up","scope":{"project":"p_<hash>","worktree":"w_<hash>"},"status":"error","dry_run":false,"results":[{"instance_id":"p_<hash>/w_<hash>/api","status":"failed","state":"stopped","steps":[]}],"errors":[{"code":"PORT_CONFLICT","instance_id":"p_<hash>/w_<hash>/api","message":"port unavailable","actions":["retry with a free port"]}]}
```

Top status: ok/error/partial; per-item status: completed/unchanged/failed/not_run.
Common fields always present: version, command, scope, status, dry_run, results,
errors. Empty results/errors use []. Discovery objects live in results, with IDs
and configuration fields but no secrets. Plans use optional plan field. Errors
have code, message, optional instance_id/field/candidates, and actions array.
Exit 0 success/help; 2 usage/config/ambiguous/not-found/cycle; 3 busy/conflict;
4 build/hook/readiness/process/IO failure; 5 partial; 130 cancellation. Stable codes:
USAGE, CONFIG_INVALID, VERSION_UNSUPPORTED, NOT_FOUND, AMBIGUOUS, CYCLE,
BINDING_INVALID, BUSY, STATE_CONFLICT, STATE_CORRUPT, PORT_CONFLICT,
BUILD_FAILED, HOOK_FAILED, READINESS_TIMEOUT, PROCESS_FAILED, OWNERSHIP_UNKNOWN,
IO_ERROR, TLS_INVALID, ROUTE_CONFLICT, TUNNEL_FAILED, CANCELLED.
No stack traces or environment dump. share may add credentials.password only
on successful creation; repeated share returns URL without password. Nothing
else exposes the plaintext secret, including dry-run, status, logs and errors.

## Proxy and publication contracts

Local machine config: $XDG_CONFIG_HOME/rundevrun/config.toml or
~/.config/rundevrun/config.toml. version=1, [routing] enabled=false by default,
domain, hostname='{service}-{worktree}-{project}.{domain}', http_listen='127.0.0.1:8080',
https_listen='127.0.0.1:8443', external_https_port=443 only when explicitly set,
trusted_proxies=[]; [[routing.tls.certificates]] cert_file/key_file.
Project may override domain in [routing] of dev.toml. Runtime stores explicit
worktree alias and per-instance hostname; alias changes require explicit command.
Default worktree label is directory name, optional branch label must be explicit.
Normalize DNS labels lowercase; reject >63-byte label, full-name collisions and
empty labels with an actionable alias error. Never resolve a hostname by parsing
its pieces. Reserve full hostname to an exact instance; no alias takeover.

Up with routing starts detached per-user proxy, except --no-proxy. Proxy only
routes registry targets whose run ID, PID identity and loopback listener ownership
match and startup gate succeeded: running without healthcheck or ready after
HTTP/TCP health. Revalidate before dialing, invalidate route on process death. 404
unknown Host, 503 stopped known instance. Bind only loopback. TLS requires valid
key pair, dates and SAN coverage for every hostname. SNI chooses provided cert;
Host must match accepted TLS name, no domain fronting. Reload validates complete
new config first and swaps atomically, keeping old config on failure. Never
fallback to plaintext. Proxy restarts don't stop apps. No DNS or CA changes.
Forward original Host and scheme, strip untrusted forwarded headers, honor only
explicit trusted local terminating proxies. ReverseProxy handles upgrades and
streaming with immediate flushing, preserves application cookies and CORS.

Live/snapshot gateway is per publication, bound loopback, with cloudflared child
tracked by identity. No general HTTP forward proxy. Every route, static file and
WebSocket handshake passes password gate. Password is random >=128 bits,
stored only as a salted verification hash; gateway session signing key private.
Use publication-specific cookie name/path, Secure/HttpOnly/SameSite=Lax, random
session key each publication; no old cookie acceptance. Login POST has same-origin
checks, bounded body, constant-time verification and throttling. No Cookie/Origin
rewrite, OAuth bypass, injected login script or CSP override. Snapshot copies
artifacts to a private staging directory, rejects escaping symlinks, activates
only after build/copy/gateway/tunnel readiness. Failure preserves existing share.
Repeat share is idempotent; --recreate prepares before replacing and rotates secret.
unshare stops only gateway/tunnel. down/switch close any publication whose root
or explicitly routed dependency is affected before stopping it. Tunnel crash
sets publication failed without stopping apps. Quick Tunnel SSE is unsupported.

## Ownership for first parallel tasks

Sol owns docs, go.mod/go.sum, cmd/rdr, and any shared contract change until
explicit handoff. 1A Luna owns internal/config and internal/discovery only;
discovery is pure, registration writes are integrated later through state APIs.
1B Terra owns internal/state and internal/procid only. No shared new type package
is required: config defines schema structs; discovery defines descriptors; state
defines persisted records. Operation integration converts them at the boundary.
Neither builder changes CLI, toolchain, docs or module files without Sol.
Terra delivers Store read/update with locked atomic registry mutation,
per-instance operation locks, run-state IO and stale reconciliation. Luna
delivers strict Load, root/worktree discovery and selector resolution. Builders
must report exported APIs and tests; Sol fixes integration before lifecycle work.
