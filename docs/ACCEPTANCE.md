# Verification and known limits

Run the core checks from the repository root with Go 1.27 or newer:

```sh
go vet ./...
go test ./...
go test -race ./...
node --test scripts/domain-wizard-token.test.mjs
```

Build the CLI and run the synthetic scenarios with Python 3, Git, and OpenSSL:

```sh
go build -trimpath -o /tmp/rdr ./cmd/rdr
python3 examples/mini/scenario.py /tmp/rdr
python3 examples/mini/switch_scenario.py /tmp/rdr
python3 examples/mini/tls_scenario.py /tmp/rdr
python3 examples/mini/share_scenario.py /tmp/rdr
python3 scripts/rdrtop_smoke.py
```

The [mini example](../examples/mini/README.md) describes each scenario and its
cleanup rules. Fixtures use disposable repositories and isolated runtime state.
If cleanup cannot confirm stopped services, they retain their files and report
the paths for inspection. The TLS fixture does not change DNS or system trust.

The optional `examples/mini/cloudflare_scenario.py` uses real outbound network
access and `cloudflared` to publish synthetic password-protected data. Run it
only when that external publication is intended.

## Limits

Earlier Linux checks covered graph lifecycle, hooks, switching, recovery,
local TLS, and live and snapshot sharing. These are historical observations,
not proof that every later revision passed. Run the checks for the revision
you intend to use.

Linux-specific process and publication tests do not execute on macOS. Darwin
cross-builds do not establish runtime correctness. Exercise process ownership,
listener ownership, and lifecycle behavior on each target operating system.

Synthetic TLS tests do not establish that a user's DNS, ingress, or browser
trust is configured. Tailnet setup status does not prove access from a second
device. Quick Tunnels do not support SSE. See [local HTTPS](LOCAL_HTTPS.md) and
[networking](NETWORKING.md) for deployment checks.
