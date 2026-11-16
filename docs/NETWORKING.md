# Networking

rdr gives registered HTTP services local URLs and can expose selected projects over a private Tailscale network. Local HTTP works without Cloudflare credentials or an active tailnet. The default listener is `127.0.0.1:8080`.

Settings live in `$XDG_CONFIG_HOME/rundevrun/networking.json`, or `~/.config/rundevrun/networking.json` when `XDG_CONFIG_HOME` is unset. Existing `config.toml` TLS routes remain supported and networking commands do not rewrite them. Runtime process state stays in the existing state directory.

## Inspect and change settings

These commands use the versioned JSON envelope and do not start a proxy or contact providers during inspection or planning:

```sh
rdr network inspect --json
rdr network setup-check --json
rdr network plan --file desired.json --json
rdr network apply --file plan.json --json
rdr network setup-plan --json
rdr network setup-apply --file setup-plan.json --json
rdr network setup-recover --json
rdr network cleanup-plan --json
rdr network cleanup-apply --file cleanup-plan.json --json
```

`inspect` returns current settings and the config revision. `check` reports locally known tailnet setup state and does not contact providers. `plan` accepts a version 1 desired settings JSON file, validates it, and returns before/after settings plus affected local route changes and the registry revision. `apply` accepts that reviewed plan JSON, checks both revisions and route changes again, reserves the proposed listener, saves settings, commits routes, then reloads the proxy. If settings or registered routes changed after planning, create a new plan.

`setup-check` reports locally known settings, ownership records, and certificate state without contacting providers. `setup-plan` contacts Cloudflare and Tailscale to check current resources and reports the exact DNS, certificate, and Tailscale Serve actions. It does not apply them. Review its output before the user directs setup. If it returns a certificate terms URL, the user must review and accept those terms. Set `terms_accepted` to true in the plan file only after acceptance. `setup-apply` rechecks the plan and applies it through the shared networking backend, then activates local routes and the proxy. Re-plan after a baseline conflict. `setup-recover` handles an interrupted operation only when it can verify ownership. If it reports ambiguity, inspect the resource and use the explicit `adopt-dns ID`, `adopt-serve`, `resolve-challenge ID`, or `resolve-finalization` operation after review. Resolving ambiguous ACME finalization may consume another certificate issuance against CA rate limits. Use `adopt-existing-dns ID` only after inspecting the exact matching DNS record and getting user confirmation. `credential-store REF` reads a token from stdin and stores it privately. Use `credential-replace REF` for explicit token rotation. Both commands print only the reference name:

```sh
host-secret-tool | rdr network credential-store cloudflare-dns-token
```

If a proxy listener reservation remains after an interrupted reload, inspect `rdr proxy status` and run `rdr proxy abort` to release it before retrying.

Changing a legacy provided-TLS listener between ports reserved by managed tailnet TLS can require a proxy restart. If proxy reload reports `TLS mode or listener addresses changed; restart proxy`, run `rdr proxy stop`, then `rdr up` or initialize `rdrtop` again. This restarts only the proxy; application processes remain running.

To change the tailnet domain or zone, first create and review `cleanup-plan`. Apply it with `cleanup-apply` only after reviewing the listed owned DNS and Serve removals. Cleanup preserves credentials and certificates. Then update the networking settings with `plan` and `apply`, and run a new setup plan for the new domain. Never remove unrelated DNS records or Tailscale Serve entries.

Example desired settings:

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

The short project address needs one selected default HTTP service and one primary main/master worktree when several eligible choices exist. Full service addresses remain available while a choice is unresolved.

## Tailnet credentials and recovery

Use a Cloudflare API token scoped to the selected zone with Zone Read and DNS Edit permissions. Store it through the host's secret-delivery mechanism in rdr's private credentials directory. The directory must be mode 0700 and the token file mode 0600. `credential_ref` contains only the file name. Never paste token values into JSON, shell arguments, logs, or skill files. Cloudflare's browser login is not an ACME credential source.

A missing or stale credential reference, conflicting DNS record, changed config revision, DNS timeout, certificate renewal failure, or unavailable Tailscale connection must be reported with its recovery action. Do not overwrite a DNS record that another owner changed. Keep local HTTP available if tailnet setup or certificate renewal fails.

For a guided conversation, use [`rundevrun-domain-setup-wizard`](../skills/rundevrun-domain-setup-wizard/SKILL.md). It supports macOS and Linux, offers rdr installation/update, checks Tailscale and Cloudflare CLI, guides domain onboarding, and asks for a project subdomain and exposure choices. It creates a scoped token through cf without printing its value and calls the same rdr backend for setup.

Install the complete `skills/rundevrun-domain-setup-wizard` directory, including its references, scripts and `agents` metadata, using your agent host's skill installation workflow. Then explicitly invoke `$rundevrun-domain-setup-wizard`, or `/rundevrun-domain-setup-wizard` on hosts using slash commands. Automatic invocation is disabled in both skill frontmatter and Codex metadata. Creating the skill in this repository does not install it globally.

The existing [`rdr-network-setup`](../skills/rdr-network-setup/SKILL.md) remains available for direct technical setup/recovery. Both skills require explicit invocation. A successful settings apply alone does not prove a browser-trusted certificate, working DNS, or access from another tailnet device.

Maintainers can check the wizard's token helper with `node --test scripts/domain-wizard-token.test.mjs`. These tests use isolated fake CLIs and do not contact Cloudflare or change machine networking.

## Ownership and migration

Local routing is always enabled and has no persistent off setting. `rdr proxy stop` is temporary; normal service startup or TUI initialization restores the local proxy. `--no-proxy` does not remove local routes.

Legacy provided and external TLS settings stay in `config.toml`. Networking settings use `networking.json`; neither operation silently migrates or deletes the legacy file. Tailnet entry URLs do not change a running process's environment. Restart a service explicitly if it needs updated canonical launch URL values after a local hostname or port change.
