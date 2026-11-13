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
MillDrum preview OAuth metadata requiring unauthenticated access. Generic password
gate must protect every path; stage 7 cannot claim old OAuth behavior is preserved
without resolving that conflict. These do not block independent stage 1 work once
contracts are accepted. Second project selected by user: /home/alex/dev/twikko.

## A0 re-check, 0478d52

Astra found no blockers to 1A/1B on the corrected contract revision. Sol accepts
stage 0. Runtime guarantees remain unimplemented and require the specified later
checks. No source repositories or application processes were changed by reviews.
