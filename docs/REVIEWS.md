# Review log

## A0, baseline 75f90a3

Astra read-only review identified ten contract defects. Sol applied the following
changes to TECH_SPEC before opening builder tasks. A second A0 review is required
because the spawn protocol changed. No lifecycle behavior is implemented yet.

| Finding | Contract correction | Later implementation verification |
| --- | --- | --- |
| 1, authorization/spawn race | Per-run spawn gate; durable accepted claim; child bootstrap cannot exec until persisted identity and acknowledgement. | Pause/crash at every handshake boundary; down must prevent late spawn. |
| 2, active operation mistaken for abandoned | Status is read-only; recovery takes instance locks and gates, then re-reads; busy never means stale. | Concurrent long mutation/recovery returns busy, preserves active operation. |
| 3, desired/effective binding conflict | Edge rule table; reject affected pending binds before build; explicit target binds win unless pointing to a removed source. | Pending bind, stopped consumer, simultaneous consumer/dependency switch. |
| 4, failed consumer stop | Explicit stop prerequisites; live consumer blocks dependency shutdown. | before_stop failure and surviving child keep dependency alive. |
| 5, old/new hook context | Private immutable stop contract records old commands/cwd/env/run_dir/bindings; start uses new preparation. | Edit config and bind, stop still uses old data-home environment. |
| 6, wildcard listeners | Verify bind address plus ownership; reject non-loopback TCP/UDP sockets of tracked processes. | IPv4, IPv6 wildcard and dual-stack on both target OSes. |
| 7, incomplete parser schema | Full field/type/default table, project routing, share and timeout rules. | Strict nested unknown keys and inter-field errors. |
| 8, shell grammar | Fixed restricted placeholder grammar, safe quoted env substitutions, concrete meanings/examples. | Spaces/quotes/dollars/newlines, quoted placeholder rejection, shell variables/literal braces. |
| 9, no-healthcheck deadlock | Startup gate separated from health state; liveness satisfies dependencies, owned loopback HTTP listener permits proxy routing. | Unchecked HTTP service stays running and serves route. |
| 10, global/per-project corruption | Strict top-level JSON with independently decoded raw project records; syntax damage global, semantic/run damage isolated. | Corrupt project record preserves healthy project updates; corrupt top-level JSON fails closed. |

Registration remains part of stage 1 acceptance. Luna provides pure descriptors;
Sol wires registration through Terra Store after both tasks are accepted. No
discovery command runs project code. JSON example already uses item status failed.
Port conflict after old-process shutdown leaves it stopped; only preparation
failure promises preservation of the old live process.

Unresolved external/product acceptance: macOS lifecycle, real TLS domain and share,
MillDrum migration. User resolved the preview conflict on 2026-11-13: protected
preview may disable OAuth; every gateway path remains password protected. Normal
project sign-in remains a migration requirement. These do not block independent stage 1 work once
contracts are accepted. Second project selected by user: /home/alex/dev/twikko.

## A0 re-check, 0478d52

Astra found no blockers to 1A/1B on the corrected contract revision. Sol accepts
stage 0. Runtime guarantees remain unimplemented and require the specified later
checks. No source repositories or application processes were changed by reviews.

## Stage 1 integration, 2f8cefa

Sol reviewed builder source and requested concrete corrections before acceptance:
real dependency URL placeholders and routes, all-service defaults before edge
validation, immutable stop fields, error propagation for uncertain identities,
nested state consistency, empty optional config fields, command validation for
hooks, path selector priority and Git environment isolation. Builders added
regression checks. Sol completed DNS domain label validation and integrated CLI
registration/listings and typed JSON errors. Stage 1 accepted on Linux after
vet/test/race and real-binary discovery checks. macOS cross-build is not runtime
acceptance. Orphan temp-file test simulates interruption, not injected disk faults.

## Intermediate stage 2A/2B, a840226

Sol integrated Terra core and Luna CLI and ran all-package test/vet/race checks
and a public-binary smoke scenario with isolated worker and HTTP services. Core
review fixed unsafe identity-error swallowing, group rechecks before signalling,
UDP client/listener distinction, immediate leader exit while supervisor lives,
fixed-port restart, bounded unresponsive-supervisor stop, config-independent stop
and log access, and CLI state-inspection error reporting.

Stage 2 remains in_progress. Required follow-up 2C must persist preparation/build
identity before executing project code and recover after frontend SIGKILL without
leaving children or losing the previous active run. Stop must not claim cleanup
when an untracked group could remain after supervisor death during shutdown.
Current fail-closed refusal for an already-dead supervisor is partial protection.
Darwin inspection currently refuses operations; cross-build passed, runtime did
not. No real MillDrum/twikko processes or data were used.

## Stage 2 closure, 301afb9

Sol accepted implementation after all-package Linux test/vet/race and public CLI
checks. Native Terra implemented gated preparation before build execution,
separate preparation intent preserving the old active run, identity/member
persistence and recovery after actual frontend SIGKILL. Sol added read-only
session/group absence checks and a late-fork regression: unknown survivors never
produce successful stopped state or release reservations. No blind group kill.

Retention keeps five confirmed unreferenced completed runs plus protected records;
seven real restarts and symlink/foreign/unknown/corrupt cases are tested. Luna CLI
exposes digest drift and preparation and retains nonempty results on cleanup IO
errors. Sol removed the obsolete untracked build path and made its timeout test
exercise the production bootstrap path.

Darwin now uses native process and socket inspection, including descriptor and
fileport snapshots, instead of the incomplete lsof approach. Header source:
https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/proc_info.h
Group/session logic and parser fixtures were inspected; Linux parser tests and
arm64/amd64 cross-builds pass. This is not runtime acceptance of Apple's private
syscall/ABI or macOS lifecycle. It remains mandatory before release.

For A1: review the conservative OWNERSHIP_UNKNOWN outcome when no verified anchor
remains, snapshot/signal races and run retention. Unknown artifacts deliberately
remain protected until an explicit safe recovery is possible.

## Stage 3A1 integration, bb5e56d

Sol reviewed graph planner/binding source and required consumer operation locks,
revision validation for concurrent cross-worktree binds, collision checks for
fixed/reserved ports, target placeholder availability before metadata writes and
immutable active addresses despite changed config. Regression suites cover these
cases, including competing bindings that would form a cycle. CLI exposes Bind
and readonly graph/binding plans. This does not yet implement graph execution.

Examples use public JSON commands and isolated Git worktrees. The scenario now
reaches multi-service up and reports INCOMPLETE, then confirms cleanup via public
status. It cannot count as the second real project or as passed stage 3.

## Stage 3 execution integration, 12a2457

Up, down and restart use one graph planner and shared preparation/activation.
Sol reviewed complete graph reservations, all-build-before-old-stop restart,
consumer stop guards, partial result reporting and cleanup. Corrections include
unknown state reporting instead of swallowed inspection errors, typed error
codes, cancellation priority and unhealthy dependency startup gates.

All-package Linux test/vet/race passed. The rebuilt public binary completed
`python3 examples/mini/scenario.py /tmp/rundevrun-stage3-rdr` with PASS and
verified cleanup. This closes the stage 3 example, not real-project, macOS,
switch or publication acceptance.

## Internal stage 5 review, integrated in fb58efe

Terra found that listener ownership accepted any loopback address, while health
and proxy dialing always use `127.0.0.1`. An owned listener on `127.0.0.2` or
`::1` therefore could coexist with an unrelated listener on `127.0.0.1` at the
same port. Port-only ownership did not protect the actual connection target.

The shared inspection now requires ownership at the actual dial address and
retains rejection of tracked non-loopback sockets. Linux also rejects foreign
listeners on that endpoint, including SO_REUSEPORT co-listeners and wildcard
listeners. Real separate-process regressions verify refusal without harming the
foreign process. Darwin rejects SO_REUSEPORT on the selected listener rather
than claiming exclusive ownership without a global socket inventory. Native
parser fixtures cover exact address, IPv4-mapped address and reuse flags.

All-package Linux test/vet/race passed after integration. Sol additionally strips
all incoming X-Forwarded-* extensions and X-Real-IP before generating accepted
forwarding metadata; tests check spoofed port/prefix and unchanged application
cookies. Include the check-to-dial race and address invariant in A1 and A2.
macOS runtime remains pending, and the handler alone is not stage 5 completion.

## Hook integration, fb58efe and 4641840

Immutable hook invocation/environment and durable identity precede project code.
Recovery verifies cleanup and requires explicit retry of failed/interrupted stop
hooks. after_start failure retains the application and blocks new consumers.
Operation IDs span the batch, and hook reason follows the actual command even
when restarting a stopped service. Sol reviewed lifecycle/recovery and ran the
all-package Linux test/vet/race checks.

A rebuilt public binary verified failed before_stop keeps the app live, ordinary
down does not replay it, --retry-hooks executes the saved old command after a
config edit, status excludes private environment, and final cleanup is confirmed.
This accepts stage 4A only. Switch, interrupted switch and independent Astra A1
remain outstanding.

## Stage 4 switch and A1 closure, a825cb3

The public binary completed `examples/mini/switch_scenario.py` on the integrated
switch CLI/core revision bacc5c9. Single, list, file and all-services mappings,
invalid-last-selector preservation, independent runs, stopped-consumer binding
updates and failed preparation were exercised. Cleanup was confirmed through
public status. All-package Linux test/vet/race passed before the A1 corrections.

Astra A1 found three defects: hook retry reached unchanged dependencies, a
revoked launch without a supervisor could remain starting, and successful switch
did not prune completed runs. Corrections in 60efe47 added node-scoped retry,
phase-aware recovery and pruning only after a durable completed journal.

The first re-check found a fourth case: accepted claim with Child=ZERO after
startup failure could never recover. Revisions 8b7b180 and a825cb3 require a full
matching supervisor identity, matching operation/run intent and verified absence
of the whole session before recording stopped. That branch sends no termination
signals and skips stop hooks. Partial identity, another boot or an unknown
survivor preserves the registry and returns OWNERSHIP_UNKNOWN. Startup failures
persist their cause after accepted claim; child identity remains durable before
bootstrap ACK. The CLI process reaps its own detached supervisor with async Wait.

A fresh Astra re-check of exact a825cb3 found no remaining confirmed P1/P2 in the
reviewed scope. Four targeted real-process tests passed, 6.474 s; those tests plus
retention protection passed under race, 7.860 s. State/procid and bootstrap ACK
tests passed from a clean git archive. The full switch/crash suite was not rerun
by that reviewer. A1 is closed; macOS runtime and stage 5/6 acceptance are separate.

## Stage 5 integration, 6bc734f

The detached proxy, private control socket, route reservations, worktree and
service aliases and CLI are integrated. Provided TLS loads keypairs; external TLS
accepts public certificates only and requires explicit loopback proxy trust and
public HTTPS port. Invalid reload keeps the active configuration. Proxy stop
does not stop applications. No DNS, certificate issuance, trust store or existing
Nginx Proxy Manager configuration was changed.

Targeted external/provided daemon and routing race tests passed, as did 39 CLI
tests, CLI vet and build. Open/doctor and a public transport scenario remain
pending. Existing local domain observations are recorded in LOCAL_HTTPS.md; they
do not demonstrate a working runner ingress.

## Post-A1 Linux integration corrections, 991efdb

Full integration tests exposed an obsolete late-fork fixture: it used an accepted
claim with no child identity, where the new recovery contract forbids signalling.
The fixture now records a real initial child in its own process group, then forks
an unrecorded survivor on TERM. The targeted test passed three times.

A full race run then observed ESRCH while reading an unrelated process stat after
that process exited. Linux procfs can return ESRCH after open succeeds. Identity,
group/session scans and descriptor snapshots now treat it as process disappearance,
alongside ENOENT. Permission failures and malformed data still fail closed.
`TestRetentionAfterSuccessfulSwitches` passed three times under race, 50.120 s;
procid race tests and runner/procid vet passed. The complete race suite has not yet
passed on this revision, so the targeted result does not replace that check.

Read-only diagnostics passed race tests. Disabled routing skips unused proxy and
pinned-host probes while keeping actual application observations. CLI open/doctor
and the public TLS scenario remain separate integration tasks.

## Diagnostics CLI integration, 1c46f79

Open and doctor adapters are integrated. The 42 CLI tests and CLI vet/build passed;
root then ran `go vet ./...` and `go test -race ./... -count=1` successfully on
1c46f79, including runner, 146.100 s. This closes the full race follow-up after the
procfs correction. New CLI and diagnostics targeted race checks also passed.

A public binary at /tmp/rdr-stage5-diagnostics verified doctor with disabled
routing returns the observation without creating runtime files, and open against
a missing project returns NOT_FOUND without creating files. JSON open never
launches a browser; its direct-URL fallback requires verified target ownership.
Doctor returns exit 4 for failed diagnostics and renders individual checks in
text and JSON. No application commands run during these reads.

The fixture is integrated in 31bb721. Luna's first attempt found a fixture-only
path-versus-ID comparison error and confirmed cleanup. After correction the
scenario passed, then Sol independently ran
`python3 examples/mini/tls_scenario.py /tmp/rdr-stage5-diagnostics` with PASS and
confirmed cleanup. It checks HTTPS dependency routing, Secure cookie roundtrip,
SSE flush before a delayed second event, valid WebSocket handshake and HMR text
frame, alias collision preservation, stopped-route 503 isolation, proxy restart
and failed certificate reload with unchanged application RunIDs. Real user-domain
ingress and macOS runtime remain outside this automated evidence.
