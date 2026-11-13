# MillDrum migration map

Read-only inspection baseline `1184e35aba0a8c09699bff66439d00ba269b8209`.
No MillDrum edits or running-process changes have been made. Recheck its AGENTS.md
and HEAD before stage 7; use its run-milldrum skill for every execution.

| Source | Destination and acceptance |
| --- | --- |
| internal/devrunner/process.go command/build/launch/waitReady/supervise | Generic process supervisor, arbitrary services, detached groups, per-run build artifacts. Add cancellation cleanup, durable pre-spawn intent and listener ownership. |
| process.go groupRefs/refreshMembers/stopPart | Generic tracked process tree shutdown. Verify PID identity, group/session and OS boot; don't assume leader survives. |
| internal/procid Observe/Matches/Terminate, identity_linux.go, identity_darwin.go | Adapt into internal/procid; add boot identity and deterministic stale PID tests. Check-to-signal race remains a documented platform constraint. |
| internal/devrunner/state.go lock/readJSON/writeJSON | Generic private atomic registry and per-instance locks; remove MillDrum home import, fixed apiAddr/uiURL and session/part roles. |
| state.go checkout/freePort | Generic Git discovery and plan-time port assignment; no MillDrum file probes or killing port owner. |
| operations.go parts/get/set/start/stop | Replace fixed backend/ui/expose singleton with instance graph and shared planner; prepare entire replacement set before stopping. |
| operations.go snapshot | MillDrum script/command only. Stop backend and agents, acquire data owner lock, then offline backup, before new backend can migrate DB. Backup failure prohibits new start. |
| internal/owner and cmd/milldrum/main.go signal shutdown | MillDrum retains data lock and graceful agent shutdown. Generic TERM deadline must allow it. Verify owner lock release before backup; SIGKILL cannot promise graceful backup. |
| process.go sourceStamp and 500ms watcher | MillDrum project watcher. Build before stop, preserve old backend on build error, delegate process ownership to runner-compatible project command. Verify restart children remain tracked. |
| seed.go snapshotSetup/seedCLI/applySetupSeed | MillDrum commands and ./dev wrapper, explicit data home, snapshot/apply preserved. No seed in runner. |
| cli.go execCLI | Wrapper delegates to selected project's built MillDrum CLI with exact environment/data home. |
| expose.go exposureCLI/printExposureCode | MillDrum sign-in and one-time codes stay project commands; never add them to generic share. |
| expose.go superviseExposure, expose_password.go | Adapt generic cloudflared lifecycle and password gate after separating app login and verifying all-path protection. |
| expose.go exposureHandler | Do not port Cookie stripping, Origin rewrite, OAuth metadata bypass, injected login.js or CSP rewrite into generic gateway. |
| dev and cmd/milldrum-dev | New ./dev checks installed compatible rdr, dispatches runner and project commands. Preserve old launcher until accepted; no simultaneous supervision. |

Current backup uses owner.AcquireLock, private staging and rename, copies WAL,
does not follow symlink targets, excludes owner.lock/owner.json/db-shm.
It also records LastBackend before a potentially migrating launch, even if launch
fails. Migration must retain this last-writer history in MillDrum data metadata;
generic run history must not silently restore an older binary against migrated DB.
Parallel backend instances get separate MillDrum data homes. Sequential shared-data
switch uses explicit hooks with old/new paths and shared data lock.

## Preview compatibility decision

The old gateway allows unauthenticated GET /v1/mcp/oauth/client/{id} and discards
all app cookies. Those rules conflict with the new all-path password contract.
Project adaptation will use a same-origin preview server in MillDrum for API
routes and sign-in, with app host-only cookies and explicit public callback URL.
It is placed behind rdr's complete password gate and receives publication URL
through the wrapper's project setup. No generic gateway OAuth exception.
An OAuth provider that requires fetching metadata without a password cannot use
this password-gated URL. Such OAuth callbacks must use a separately configured
project endpoint or be reported unavailable in protected preview; do not silently
bypass the gate. Stage 7 must reproduce the current daily preview scenarios and
surface any unresolved product conflict to the user before acceptance.

## Migration checks still required

Use isolated runtime and copied disposable data first. Compare old/new commands
for start, down, status, logs, worktree discovery, restart/build failure, watcher,
agent termination, backup order/failure, seed snapshot/apply, sign-in/exec and
snapshot preview with cookies and OAuth. Only then explicitly stop old launcher
before new daily processes. Keep rollback instructions and old launcher; do not
roll back schema/data automatically. No live data is a fixture.
