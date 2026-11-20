# Private HTTPS for dev runs

Configure a user-owned subdomain for private HTTPS access to rundevrun projects. Run
only when explicitly invoked. Speak the user's language. Ask one short question
at a time, using choices where useful; reuse answers already given and settings
already verified. Execute the steps you can perform, and guide the user through
account login, registrar access and other steps requiring their participation.

Keep this a conversation, not a generated shell wizard. All project DNS,
certificate and Serve changes go through `rundevrun network` plan/apply operations.
Cloudflare CLI handles discovery, authorization and scoped-token creation.

Detect the OS read-only before provider changes. On Windows or another
unsupported platform, explain that this version supports macOS/Linux and stop
before migrating DNS or installing tools.

## 1. Find the user's starting point

Run `rundevrun network inspect --json` and `rundevrun network setup-check --json`
to identify the current domain, selected projects and locally known setup state.
Reuse verified settings and skip completed steps. If setup was interrupted or
the user wants to switch domains, start with [recovery and cleanup](#recovery-and-cleanup).
If the requested setup is already complete, go to [verification](#5-verify-and-hand-over).

For a new setup, ask whether they own a domain and use Cloudflare DNS:

- I own a domain; it is not connected to Cloudflare.
- I own a domain; it is connected to Cloudflare.
- I do not own a domain.

If they have no domain, explain that this custom-domain workflow requires one.
Local `.localhost` URLs still work. End this path without buying a domain or
installing tools. They can invoke the wizard again after obtaining a domain.

Otherwise ask for the domain name. A domain alone is sufficient here; collect
the desired project subdomain later. Do not reuse names, emails, addresses or
resource IDs from an example, the author's machine or a previous user.

For a domain not using Cloudflare, read
[Cloudflare onboarding](references/cloudflare-onboarding.md). Find and open the
current official instructions and guide the user through the steps. Resume this
wizard only once the zone is active and public delegation matches Cloudflare.
If propagation is pending, report the pending step and how to resume; do not
keep polling indefinitely or start certificate issuance.

For a domain already connected, explain the intended result before installation:
DNS-only wildcard pointing to this computer's Tailscale IP, a trusted wildcard
certificate with renewal while the proxy runs, and private TCP Serve forwarding.
Local HTTP remains available. Verify the claimed Cloudflare state after login.

## 2. Check and prepare tools

Read [prerequisites and authentication](references/prerequisites.md) for this
stage, using the instructions for the detected macOS or Linux system.

1. Find rundevrun and inspect `rundevrun version --json`, `rundevrun network inspect --json` and
   `rundevrun network setup-check --json`. Recommend installing/updating rundevrun when it is
   absent or lacks the required commands. Offer to perform that work from a
   trusted checkout/release using its documented installation procedure. Obtain
   consent for installation/update; preserve existing configuration and binaries.
2. Check Tailscale installation, daemon status, login and this machine's tailnet
   IP. If missing, offer installation and fetch the official instructions for
   this OS/distribution. Help the user join the intended tailnet. Continue only
   when `tailscale status --json` reports `Running` and an assigned tailnet IP.
3. Check the official Cloudflare `cf` CLI and a compatible Node runtime. If
   missing, offer installation, then prepare OAuth login and zone discovery.
   Follow the reference for creating and privately storing a scoped DNS token.

If the user declines a prerequisite, say which remaining step needs it and stop
that path. Do not silently substitute a different remote-access provider.

## 3. Choose addresses and projects

Ask which subdomain they want for this computer's projects. Offer `dev` as an
example, expanding it using their domain, such as `dev.example.com`. Confirm the
resulting full base domain rather than guessing whether their answer is a label
or a complete hostname. Use a subdomain, preserving the zone apex and existing
production routes. Inspect the exact proposed wildcard record for conflicts.

Explain in plain language:

- Devices can open these project URLs while connected to the same tailnet and
  allowed by its access policy. Phones and other computers need Tailscale too;
  sharing a Wi-Fi network alone is insufficient.
- The URLs reach projects on this computer while it, the applications and proxy
  are running. Renewal cannot run while the computer is asleep or off.
- This release uses one computer per base domain. Another computer needs a
  distinct subdomain; never repoint an existing wildcard to a new machine.
- DNS is public, but the selected address is a private tailnet IP. This setup
  uses Serve, not Funnel or a public tunnel.

List registered projects with `rundevrun projects --json` and their services/worktrees.
Ask which projects should be accessible; do not expose all projects by default.
New worktrees of a selected project inherit its exposure. When a short project
URL has several eligible HTTP services or main/master worktrees, ask for the
default service/primary worktree and save stable IDs returned by rundevrun. Keep full
service URLs alongside the alias. If no projects exist, explain how to register
an HTTP service, and resume after the user chooses one.

Ask for the email to register with the CA. Reuse an existing email if confirmed.
Build desired settings from the current inspected configuration, changing only
the selected domain/provider and project choices. Preserve unrelated settings.

## 4. Preview, then apply

Use the installed CLI's `--help` and JSON envelopes as the command/schema source.
Typical flow:

```sh
rundevrun network plan --file desired.json --json
rundevrun network apply --file settings-plan.json --json
rundevrun network setup-plan --json
rundevrun network setup-apply --file approved-setup-plan.json --json
```

Save the returned **`plan` member**, not the whole envelope, as each apply file.
Keep temporary plans in a private directory. They contain settings and credential
references, never secret values. Keep tokens out of arguments, logs and chat.
Never inspect Cloudflare CLI credential files. Show the settings and concrete route/alias
changes and obtain approval before applying the settings plan. Before first issuance, a settings
apply can report `partial` because no managed certificate exists yet; inspect
the result to distinguish saved settings/pending TLS from other failures.

Show the resulting setup plan: exact wildcard name/IP/proxy status, certificate
scope, existing resources affected and Serve forwarding. Obtain the user's
direction to apply this concrete external plan. Present its exact `terms_url`
and get explicit CA-terms acceptance before setting `terms_accepted=true`; combine
these questions where practical. Existing authorization remains valid for the
same plan/terms; elapsed time or silence is not acceptance. If a revision or
resource changes, inspect and regenerate the plan rather than replaying it.

If already configured, verify and report unchanged resources instead of issuing
another certificate or recreating DNS. For an existing unmanaged record, show
its identity and values. Use the backend's explicit adoption operation only
after the user authorizes adoption and the record meets its checks. A conflict
that cannot be adopted needs a new subdomain or the user's reviewed resolution.

If the plan needs a token, use secret delivery and pipe it to
`rundevrun network credential-store cloudflare-dns-token`. For rotation use
`credential-replace` with the same stdin delivery. Save only the reference as
`credential_ref` and generate a fresh plan.

## 5. Verify and hand over

Check `rundevrun network setup-check --json`, `rundevrun proxy status --json`, the actual DNS
record and `tailscale serve status --json`. Verify DNS-only status, the selected
tailnet IP, the certificate expiry, intended raw forwarding and challenge cleanup.
`setup-check` alone is locally known state, not remote acceptance evidence.

Open a real selected service's HTTPS entry URL using normal system TLS validation,
including its owned reported path/query/fragment. A bare origin can return an
application 404 when the application requires a session path; keep that path in
the delivered link. Check local HTTP and unchanged application RunIDs too.
Never use insecure TLS flags or change system trust to obtain a pass.

Ask the user to try the URL from another authorized tailnet device when one is
available. Report this check as unverified until there is actual evidence;
separate same-computer HTTPS from second-device/policy acceptance.

Finish with working links, selected projects/alias target, renewal requirements
and any pending checks/recovery step. Leave healthy apps/proxy running. Invoking this guide does not authorize buying domains, changing tailnet policy
or creating public exposure.

## Recovery and cleanup

If setup reports an interrupted operation, run `rundevrun network setup-recover --json` first. When recovery reports an ambiguous resource, inspect it using provider status and ask the user before resolving it with `adopt-dns ID`, `adopt-serve`, `resolve-challenge ID`, or `resolve-finalization`. Pass only the resource ID returned by the provider. Resolving an ambiguous ACME finalization may consume another certificate issuance against CA rate limits. Do not adopt or delete a resource based on its name alone.

For an existing wildcard DNS record, do not adopt it automatically. Inspect the record ID and verify it is the intended DNS-only A record for the current Tailscale IP. After the user confirms, run `rundevrun network adopt-existing-dns ID`. If a proxy listener reservation remains after an interrupted reload, inspect `rundevrun proxy status`, then run `rundevrun proxy abort` to release the pending reservation before retrying.

If proxy reload reports `TLS mode or listener addresses changed; restart proxy`, run `rundevrun proxy stop`, then `rundevrun up` or initialize `rundevtop` again. This restarts the proxy and keeps application processes running.

To change the configured tailnet domain or Cloudflare zone, first run `rundevrun network cleanup-plan --json` and show its exact DNS and Serve removals. Get the user's direction, then apply that reviewed plan with `rundevrun network cleanup-apply --file cleanup-plan.json --json`. Cleanup removes only verified owned DNS and Serve forwarding. It preserves credentials and certificates. Then plan and apply the new domain settings, save its token reference if needed, and run a fresh setup plan. Do not edit ownership records manually.
