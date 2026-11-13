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
process liveness, reported running rather than ready. HTTP checks require http
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
configuration, register, list, status or dry-run. Do not interpolate arbitrary
values into shell source. Substitute each `{...}` with a generated environment
variable expansion whose value is passed in env, with safe shell word semantics.
The chosen implementation must support concatenation such as `{run_dir}/api` and
values containing spaces, quotes, dollar signs and newlines without shell injection.
Reject placeholders inside shell quotes or other syntactic contexts that cannot
preserve these semantics; errors identify the field. Literal braces use `{{`/`}}`.
Ordinary shell `$VAR` remains shell syntax. Config env strings substitute raw
values without shell evaluation. Unknown or unavailable placeholders fail before
any build. Valid names: host, port, direct_url, url, run_dir, worktree,
project, service, dependencies.<declared-name>.{host,port,direct_url,url}.

Environment order: inherited env excluding RDR_*, configured service env,
then computed RDR_* and internal substitution variables. Config cannot assign
RDR_* or internal substitution names. No automatic PORT/API_URL mapping.
RDR_HOST=127.0.0.1; RDR_PORT only for port services; RDR_DIRECT_URL only HTTP;
RDR_URL is routed HTTPS URL or direct HTTP URL when routing is disabled.
RDR_DEP_<NORMALIZED_NAME>_* follows the same availability rules.
Build, start and hooks receive the same planned addresses. Hooks additionally
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
registry.json schema=1 contains revision counter, projects, instances, desired
bindings, reservations and operation intents. Each run has a random ID and
runs/<id> containing manifest.json, supervisor.json and logs. Supervisor alone
writes supervisor.json. CLI alone updates registry under its lock.

Persist JSON by unique same-directory temporary file, chmod, write, fsync,
close, rename and parent directory fsync. Decode errors fail closed with path,
never replace corrupt state with empty state. Reads need no long operation lock.
Separate project corruption must be reported per project when listing.

Nonblocking flock operation locks per instance, acquired in sorted ID order for
the entire affected graph, including effective consumers and selected targets.
Global registry lock is held only during read-modify-write and port assignment,
never while building, waiting, invoking hooks or calling supervisors. Lock order:
sorted instance locks, then registry lock; supervisor never takes instance locks.
After locks, re-resolve graph and revisions, retry planning or report conflict.
Locks release on process exit. Supervisor has its own per-run writer lock.

States are stopped, starting, running, ready, unhealthy, failed. Intent phase is
separate: preparing, prepared, spawning, started, stopping, stopped, hook_failed,
complete, interrupted. Write intent with run ID and manifest BEFORE spawn.
Manifest includes addresses, effective bindings, digest and operation ID.
Spawn detached rdr internal supervisor with run ID and root, not shell code in
argv. Child supervisor writes its own kernel identity before spawning application;
it must check under registry lock that its intent is still authorized. Recovery
can revoke an unacknowledged intent under that same lock, so late supervisors
cannot spawn after a recovered stop. Supervisor records child identity before
announcing startup. CLI handshake timeout triggers cleanup/revocation, never a
second untracked spawn. Supervisor survives CLI exit, starts a separate session,
handles TERM and child exit; it never automatically crash-restarts the service.

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
inspection must fail closed if listener ownership cannot be established.

Recovery joins registry intent and per-run state, verifies OS identity and marks
stale runs stopped/failed without signalling foreigners. Uncertain live ownership
is an explicit error. No generic data rollback or replay of interrupted hooks.
After interrupted switch, expose step journal and require a new explicit command.
Do not silently re-run a migration or after_stop hook.

Port assignment occurs for the entire graph before any build, under registry
lock, excluding all live/reserved ports across projects and checking loopback
availability. Bind sockets may be released before external programs start;
registry reservation protects only against rdr, not arbitrary programs. On bind
conflict fail, clean owned new processes, preserve old runs, and report conflict.
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

up starts dependencies in topological order, waits readiness before consumers;
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
routes registry targets whose run ID, PID identity, listener ownership and ready
state match; revalidate before dialing, invalidate route on process death. 404
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
