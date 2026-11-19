# Configuration and contributor reference

Use `rundevrun --help` for command syntax and the [mini project](../examples/mini/rundevrun.toml)
for a runnable configuration. Application data, migrations, and authentication
belong to each project.

## rundevrun.toml schema

Unknown fields at every depth and unknown versions are errors with filename and
field path. Service/project names match `[a-z][a-z0-9_-]*`. Empty services fail.
The table lists service fields without the `services.<name>.` prefix.

| Field | Type and requirement | Default and constraints |
| --- | --- | --- |
| version | required integer | Exactly 1. |
| project | required table | Required nonempty name, optional env_files. |
| project.env_files | optional string array | [], opted-in dotenv paths relative to the checkout root; absolute paths and paths escaping the root are rejected. |
| services | required table of service tables | At least one service. |
| routing | optional table | Only domain, a nonempty DNS domain override. Machine listener/key settings forbidden here. |
| service.command | required nonempty string | Whitespace-only rejected. |
| service.cwd | optional nonempty string | '.', confined to existing worktree directory. |
| service.build | optional nonempty string | No build if absent. |
| service.protocol, port | optional string, integer or 'auto' | Rules below. |
| service.singleton | optional boolean | false; one active instance per project/service across linked worktrees, with automatic handoff on up. |
| service.depends_on | optional string array | [], unique existing service names. |
| service.env | optional string map | {}, valid shell variable names, no RDR_* or __RDR_* keys. |
| service.build_timeout/start_timeout/stop_timeout/hook_timeout | optional duration strings | 5m/30s/20s/60s, positive. |
| service.health | optional table | Exactly one method, optional timeout defaulting to start_timeout, never greater. |
| service.hooks | optional table | Only four hook names below, nonempty command values. |
| service.routes | optional table array | [], required path and dependency strings, optional boolean strip_prefix=false. |
| service.share | optional table | HTTP only; required mode=live or snapshot; auth defaults to password, no other auth accepted. |
| share.build, share.dir | optional nonempty strings | Both required for snapshot and forbidden for live. dir relative to cwd. |
| share.env | optional string map | {}, snapshot-only, same reserved-name restrictions as service.env. |

Mode selection uses explicit --mode, then configured share.mode, then live.
Absent share permits explicit live sharing of HTTP; snapshot requires configured
build/dir. --mode live may override configured snapshot without using snapshot
build/env/dir; --mode snapshot without snapshot config fails. Empty share, health
or routing tables are invalid. Empty optional command strings are invalid, not
omission. Empty hooks/env/routes are valid. Snapshot dir may not exist at config
load: validate lexical confinement and closest existing ancestor now, revalidate
full canonical confinement after build. cwd must exist at load.



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
require a port. Hooks allow before_start, after_start, before_stop, after_stop strings only.
Dependencies must exist in the configuration and be unique; static cycles fail.
Dependency names normalized to uppercase with nonalphanumerics replaced by '_'
must be unique. routes require HTTP and a declared HTTP dependency. Prefix paths
start with '/', reject query/fragment and duplicate prefixes; longest path match
on a segment boundary wins. No arbitrary upstream URL is accepted.

Snapshot build URLs require one unambiguous declared route prefix per dependency;
multiple prefixes for the same dependency are rejected before snapshot build.
Local and live routing still support multiple prefixes.

cwd must remain inside the canonical worktree after symlink resolution. Snapshot
dir resolves relative to cwd and must remain there after symlink resolution.
Build outputs belong in run_dir; project builds that write source directories
must publish by copying into a fresh snapshot directory, never serve source dist.
Configuration digest is SHA-256 of raw rundevrun.toml bytes; live digest is immutable.

## Commands and environment

Commands run through `/bin/sh -c` in `cwd`. Configuration checks, discovery,
status, and dry-runs never execute project commands.

| Placeholder | Value |
| --- | --- |
| `{host}`, `{port}` | Loopback host and assigned port; port requires a port service. |
| `{direct_url}`, `{url}` | Direct HTTP address and canonical local entry URL; HTTP only. |
| `{run_dir}` | Private directory for this run; use it for build outputs. |
| `{worktree}`, `{project}`, `{service}` | Worktree path and configured names. |
| `{dependencies.NAME.FIELD}` | Selected declared dependency's host, port, direct_url, or url. |

Placeholders in commands must be outside shell quotes. rundevrun passes values through
quoted environment substitutions, so `{run_dir}/api` handles spaces safely.
Commands with placeholders reject backticks, command substitution, heredocs,
unquoted backslashes and comments. Plain `$VAR` and `${VAR}` remain shell syntax;
`{{` and `}}` escape literal braces. Environment values expand without a shell.
Unknown or unavailable placeholders fail before build.

Environment precedence is inherited values, main-checkout env files, worktree
env files, service `env`, then computed `RDR_*` values. Configured keys cannot
use `RDR_*` or `__RDR_*`.
Computed values include `RDR_HOST`, `RDR_PORT`, `RDR_DIRECT_URL`, `RDR_URL`, and
`RDR_DEP_<NORMALIZED_NAME>_*` where applicable. No automatic `PORT` mapping exists.
Hooks also receive reason, instance, old/new worktree, and operation IDs.
Stop hooks use the run's saved commands, cwd, environment, and bindings even if
configuration changes. Snapshot `share.env` overrides service environment;
its dependency URLs must use declared publication routes, not local addresses.

### Shared env files across worktrees

Opt in once in the shared or external configuration:

```toml
[project]
name = "example"
env_files = [".env", ".env.local"]
```

All services receive these values during builds, launch, and hooks, including
snapshot builds. Paths resolve from the checkout root, independently of service
`cwd` or the external config's location. For each checkout, later files in the
list override earlier ones. All main-checkout files load before all worktree
files, so a worktree `.env` overrides even the main checkout's `.env.local`.
Empty values explicitly replace earlier values.

The main checkout is Git's primary working tree, regardless of its branch name.
It is read once when selected directly. Without Git, or with a bare repository
that has no primary working tree, only the selected directory's files load.
Missing files are skipped. Existing files and symlinks are read without creating
or changing files in either checkout. Without `env_files`, no files are loaded
by rundevrun.

Files use dotenv syntax, parsed with [godotenv](https://github.com/joho/godotenv),
including comments, `export`, quoted values, and multiline strings. Variable
references expand against earlier entries within the same file only. Files are
data, never shell scripts; rundevrun placeholders are not expanded in them.
Malformed files, invalid shell variable names, reserved `RDR_*`/`__RDR_*` keys,
NUL values, and non-regular files fail config checks and startup planning.
Parse errors identify the file without including its contents.

Env values are saved in the run's private manifest for launch and stop hooks.
They are excluded from status and dry-run output. File contents are not part of
the TOML configuration digest. Restart services after editing env files to load
new values; existing runs and their stop hooks retain the saved environment.

## Identity and lifecycle

### Singleton services

Set `singleton = true` on a service to keep one active instance across linked
worktrees of the same project. The default is false. This setting is independent
of port selection and also works for process services. Separate clones have
separate project identities.

When `up` selects another worktree, it prepares the target, stops the previous
instance, then starts the target and checks readiness. Fixed ports can transfer
from the source to the target without changing app URLs. Existing switch rules
apply to affected consumers and dependency bindings. A request selecting several
worktrees of the same singleton service is rejected. Starting the current
instance again is unchanged. Concurrent starts of the same project/service
return BUSY while a start or switch is in progress.

Dry-run JSON includes a `switch` plan with source/target mappings, affected
consumers, and ordered steps. Executed handoffs report operation IDs, journal
paths, and step results through the usual lifecycle output.

Preparation failure preserves the source. A stop failure blocks the target.
Failure after stopping can leave the service down and is reported as partial;
there is no automatic restart of the old instance. Unmanaged processes and
unverifiable ownership remain conflicts. `restart` of a stopped worktree that
requires a handoff directs the user to `up`.

### Project identity

Projects and worktrees use hashes of canonical paths, not display names or remote
URLs. Separate clones remain distinct. Selector precedence is ID, canonical path,
then unique label. rundevrun discovers existing worktrees; it does not create them.
Shared mode loads each worktree's own `rundevrun.toml` with the same project name.
External mode loads one personal config for all linked worktrees, resolving
service paths against each worktree independently. An external config takes
precedence over repo configs. See [project setup](ONBOARDING.md) for storage,
import, and mode selection.

`bind` changes desired dependencies; running environments change only on restart.
Missing selected dependencies never fall back to another worktree. `up` starts
in dependency order. `down` rejects live consumers unless included or cascaded,
and stops consumers first. Stopping a consumer does not stop its dependencies.
Restart and switch prepare replacements before stopping old processes.

Switch moves selected live services and affected consumers. Pending binding
changes must be resolved first; explicit target bindings are preserved or rejected
if invalid. Preparation failure preserves old processes. Failures after stopping
can leave a partial result; rundevrun does not roll back application data or restart old
binaries automatically. Failed saved stop hooks require explicit `--retry-hooks`.
Shares close before affected services stop and do not reopen automatically.

## Process and state invariants

Runtime storage uses `RDR_HOME`, then `$XDG_STATE_HOME/rundevrun`, then
`~/.local/state/rundevrun`. Directories are private and state writes are atomic.
Corrupt or uncertain state fails closed; application data must live elsewhere.

- Acquire instance locks in ID order, then run spawn gates, then the registry
  lock. Never hold the registry lock during builds, hooks, or readiness waits.
- Persist authorization and process identity before allowing project code to run.
  Supervisors survive CLI exit and do not automatically restart crashed services.
- Verify PID, kernel start identity, process group, session, and boot identity
  before signalling. Never kill a process just because it owns a port.
- Readiness requires verified process and listener ownership on loopback.
  Non-loopback listeners fail startup. Processes escaping their session are
  unsupported; unknown survivors retain state and reservations for inspection.
- Status is read-only. Recovery runs under mutation locks, preserves active
  owners, and never silently replays interrupted hooks.
- Retention protects live, referenced, unresolved, and unverifiable runs. It
  never removes application data or follows directory symlinks.

## Networking internals

`internal/networking` owns versioned settings and provider operations.
[Networking setup](NETWORKING.md) documents commands, credentials, and recovery.
Plans carry baseline digests and resource identities; apply rechecks them under
locks. Cleanup removes only verified owned DNS and Serve resources while retaining
credentials and certificates. Ambiguous ownership requires explicit resolution.

Settings apply validates candidate routes and reserves the listener before writing.
Route commits require the expected registry revision. A conflict rolls settings
back only if they still match that apply. After route commit, reload failure is
partial and retryable; `rundevrun proxy abort` releases abandoned listener reservations.
Local route changes never rewrite a running process's saved launch environment.
Tailnet entry changes alone do not require an application restart.

Proxies route only to verified live loopback targets. Reload validates the whole
candidate before swapping it; invalid TLS never falls back to plaintext. Proxy
restart leaves apps running. Existing provided/external TLS settings remain in
`config.toml`; networking operations do not rewrite them.

Every live or snapshot publication route, including WebSocket handshakes, passes
the password gate. Passwords are stored as salted hashes; sessions use private
signing keys and Secure/HttpOnly/SameSite cookies. No OAuth or path bypass exists.
Snapshots copy only regular files and directories into private storage, rejecting
symlinks and special files. Preparation failure preserves the old share; a cleanup
failure after replacement returns the new active share with a partial error.

## JSON results

`--json` emits one versioned object with `command`, `scope`, `status`, `dry_run`,
`results`, and `errors`. Top status is `ok`, `error`, or `partial`; item status is
`completed`, `unchanged`, `failed`, or `not_run`. Errors include a stable code,
message, and recovery actions. Unknown process state is reported explicitly.
Progress goes to stderr; status excludes environment values and credentials.
Creating a share returns its password once, including in JSON.

Exit codes: 0 success/help, 2 usage/config/selection, 3 busy/conflict,
4 execution/IO/diagnostic failure, 5 partial, 130 cancellation.
`logs --json --follow` is rejected; follow streams text instead.
