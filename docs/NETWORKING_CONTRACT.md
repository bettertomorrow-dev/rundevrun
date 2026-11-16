# Networking contract

`internal/networking` owns desired networking settings and external tailnet
setup. `Inspect(ctx)`, `Plan(ctx, desired)` and `Apply(ctx, plan)` read and write
`networking.json`. Plans carry a baseline digest and desired settings, never
credentials. Apply rechecks the digest under a per-machine lock. A stale plan
returns `ErrConflict` and must be regenerated.

`Check(ctx)` reports locally known setup state without contacting providers.
`PlanSetup(ctx)` contacts Cloudflare and Tailscale to inspect resources and
returns DNS, certificate, and Serve changes without mutating them. `ApplySetup`
rechecks configuration and provider state, requires explicit ACME terms
acceptance when issuance needs it, and records each mutation for recovery.
Credential bytes enter through stdin in the CLI and are stored in private files;
JSON and plans contain only credential references. `ReplaceCredential` is the
explicit rotation path. `RecoverSetup` clears only operations whose ownership
can be verified. Ambiguous resources require explicit adoption or resolution
using provider resource IDs. `AdoptExistingDNS` accepts only an exact wildcard
DNS-only A record matching the configured domain and current Tailscale IPv4.
Proxy renewal uses `RenewIfNeeded` only for an existing owned setup and stops
for new ACME terms.

`PlanCleanup(ctx)` previews removal of only the recorded DNS record and owned
Serve forwarding. `ApplyCleanup(ctx, plan)` verifies that exact preview again,
then removes those resources while retaining the credentials and certificate.
Use cleanup before changing the configured domain or zone; setup never invokes
cleanup implicitly.

The default root is `$XDG_CONFIG_HOME/rundevrun`, or
`~/.config/rundevrun`. `networking.json` is version 1, with `local.listen`,
`tailnet`, and `projects` keyed by stable project ID. `local.listen` defaults
to `127.0.0.1:8080`, and has no off switch. A project setting has `tailnet`,
`default_service` and `primary_worktree`. Select the only eligible service or
main/master worktree automatically; if there are multiple choices, require a
saved choice. Never replace an unavailable choice with another target.

`tailnet.credential_ref` is a file name under a private credentials directory,
not a token. Supply a Cloudflare API token scoped to Zone Read and DNS Edit
through host secret delivery on stdin. The CLI stores it in a mode 0600 file
under a mode 0700 directory. The `cf` browser login is separate and must not be
treated as an ACME credential source. JSON, logs, process arguments, plans and
fixtures must not contain token values.

Existing `config.toml` version 1 is read using `routing.Load` and is never
rewritten by networking config operations. Existing provided or external TLS
routes continue using their legacy config and retain `Hostname` and `RouteURL`
meanings. Missing networking config does not require or create any files, and
always gives local HTTP defaults even when legacy routing is disabled. The
versioned `state.Instance.Endpoints` addition records scope, hostname, port,
instance ID and alias status. Instance IDs include the project identity, so
independent clones are not identified only by their display names. Route
assignment must reject hostname collisions across all endpoints atomically.

New process manifests use the canonical full local URL for own and dependency
launch addresses. `RouteURL` and endpoint routes describe entry addresses and
must not rewrite existing manifests or process environments. Tailnet changes
do not set `RestartRequired`; local canonical name or port changes should
retain the existing launch values and signal that an explicit restart updates
them. The normal open action uses local; tailnet is separate. URL path, query,
and fragment can be transferred only from a verified reported loopback origin.

`--no-proxy` cannot disable local HTTP availability. `proxy stop` is temporary;
normal up or TUI initialization restores local routing. `network inspect` and
`network check` do not start the proxy or contact providers. `network setup-plan`
is read-only but contacts Cloudflare and Tailscale. `network setup-apply` is the
explicit external mutation path and records partial steps for recovery.
Provider status does not prove access from a second tailnet device; that remains
a separate acceptance check.

For `network apply`, the CLI derives candidate routing from the proposed config
and validates it with `PlanLocalRoutes` before writing settings. If a proxy is
running, it reserves the proposed listener with `PrepareReloadTo` first. After
the settings write, it commits the route candidate only if the registry
revision still matches. A route-commit conflict rolls settings back only if
they still have the revision written by that apply. Once routes commit, a proxy
reload failure is reported as partial and can be retried with `rdr proxy reload`.
An abandoned listener reservation can be released with `rdr proxy abort`.
Changing legacy provided-TLS listener addresses across the managed TLS listener
ports may require a proxy restart. On the explicit reload error, use `rdr proxy
stop`, then `rdr up` or initialize `rdrtop` again. This keeps application
processes running. Preserve external TLS trust checks and legacy routes during
migration.
