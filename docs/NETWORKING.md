# Networking

HTTP services get local URLs through `127.0.0.1:8080` without provider credentials.
Selected projects can also use private Tailscale HTTPS addresses with Cloudflare
DNS. Settings live in `$XDG_CONFIG_HOME/rundevrun/networking.json`, or
`~/.config/rundevrun/networking.json`.

## Guided setup

Run `rundevrun guide domain` from any directory for private HTTPS setup,
verification, recovery, or a domain change. An agent can follow the instructions
without installing a skill. Retrieval leaves provider settings untouched.

Use `--no-update` to skip the update check. To freeze the guide, copy its
reported `guide_dir` in full to `$XDG_CONFIG_HOME/rundevrun/guides/domain.local`,
or `~/.config/rundevrun/guides/domain.local` when unset. The CLI prefers
that copy while it continues checking for updates. Remove it to use the downloaded
or bundled guide again.

## Settings and provider setup

```sh
rundevrun network inspect --json
rundevrun network setup-check --json
rundevrun network plan --file desired.json --json
rundevrun network apply --file plan.json --json
rundevrun network setup-plan --json
rundevrun network setup-apply --file setup-plan.json --json
```

`inspect`, `setup-check`, and settings `plan` read local state without contacting
providers. Settings `apply` validates the reviewed plan, updates routes, and
reloads the proxy. Re-plan if settings or registry revisions changed.

`setup-plan` contacts Cloudflare and Tailscale without changing them. Review its
DNS, certificate, and Serve actions before `setup-apply`. If issuance requires
ACME terms acceptance, review the terms and set `terms_accepted` only afterward.
Provider status alone does not prove browser trust or access from another device.

Example `desired.json`:

```json
{
  "version": 1,
  "local": { "listen": "127.0.0.1:8080" },
  "tailnet": {
    "domain": "dev.example.com",
    "zone_id": "your-cloudflare-zone-id",
    "credential_ref": "cloudflare-dns-token"
  },
  "projects": {
    "stable-project-id": {
      "tailnet": true,
      "default_service": "web",
      "primary_worktree": "main-worktree-id"
    }
  }
}
```

Short project URLs need a default HTTP service and primary main/master worktree.
When several are eligible, save a selection; full service URLs remain available.

## Credentials and recovery

Use a zone-scoped Cloudflare token with Zone Read and DNS Edit permissions.
Deliver it through stdin to `rundevrun network credential-store cloudflare-dns-token`;
use `credential-replace` for rotation. Both print only the reference name.
Credentials use mode 0600 files in a mode 0700 directory. Cloudflare browser login
is separate. Never put tokens in chat, arguments, plans, JSON, or logs.

After an interrupted setup, use `rundevrun network setup-recover --json`. Ambiguous
resources require inspection and explicit `adopt-dns ID`, `adopt-serve`,
`resolve-challenge ID`, or `resolve-finalization`. Finalization recovery may
consume another certificate issuance. `adopt-existing-dns ID` requires review and
confirmation of the exact matching record. Never overwrite another owner's DNS.

Before changing the domain or zone, review `rundevrun network cleanup-plan --json`,
then apply with `rundevrun network cleanup-apply --file cleanup-plan.json --json`.
Cleanup removes only verified owned DNS and Serve forwarding, retaining credentials
and certificates. Then apply new settings and generate a fresh setup plan.

## Proxy and existing TLS

Local routing stays enabled; `--no-proxy` does not remove routes. `proxy stop` is
temporary: normal `up` or TUI initialization restores it. Tailnet changes do not
rewrite running environments; restart services to apply changed local launch URLs.

Existing [provided/external TLS settings](LOCAL_HTTPS.md) remain in `config.toml`.
If reload requires a restart, run `rundevrun proxy stop`, then `rundevrun up` or `rundevtop`.
Apps keep running. An abandoned listener reservation can be released with
`rundevrun proxy abort`. See [contributor notes](TECH_SPEC.md#networking-internals)
for apply conflicts and partial failures.
