# Local HTTPS

For managed Cloudflare DNS, certificates, and private Tailscale access, use
[networking setup](NETWORKING.md). This page covers existing TLS infrastructure.

Machine settings live in `$XDG_CONFIG_HOME/rundevrun/config.toml`, or
`~/.config/rundevrun/config.toml`. Keep certificates and private keys outside
the repository. Project `rundevrun.toml` files must not contain keys.

## Provided certificates

Configure an existing certificate and key readable by the user running rundevrun:

```toml
version = 1
[routing]
enabled = true
domain = "dev.example.com"
http_listen = "127.0.0.1:8080"
https_listen = "127.0.0.1:8443"
[routing.tls]
mode = "provided"
[[routing.tls.certificates]]
cert_file = "/absolute/path/fullchain.pem"
key_file = "/absolute/path/privkey.pem"
```

Configure DNS separately so route names resolve only to loopback. A wildcard
certificate must cover names such as `api-main-example.dev.example.com`.
This example uses HTTPS port 8443. An existing forwarder from port 443 can use
`external_https_port = 443` in `[routing]`; rundevrun does not create the forwarding.

## Existing TLS proxy

If a trusted local proxy terminates HTTPS, use its public certificate without
giving rundevrun its private key:

```toml
version = 1
[routing]
enabled = true
domain = "dev.example.com"
http_listen = "127.0.0.1:8080"
external_https_port = 443
trusted_proxies = ["127.0.0.1"]
[routing.tls]
mode = "external"
[[routing.tls.certificates]]
cert_file = "/absolute/path/public-fullchain.pem"
```

External mode forbids `key_file`. The proxy must preserve Host, send exactly one
`X-Forwarded-Proto: https`, and connect from an explicitly trusted loopback
address. A container bridge or LAN address does not satisfy that requirement.
Update the public certificate file when the terminating proxy renews it.

## Apply and verify

Normal `rundevrun up` starts the required proxy. Inspect it with
`rundevrun proxy status --json`. Preview a reload with
`rundevrun proxy reload --dry-run --json`, then apply with `rundevrun proxy reload --json`.
Invalid configuration leaves the old configuration active. Listener or TLS mode
changes require `rundevrun proxy stop` followed by `rundevrun up`. Stopping the proxy leaves
application processes running.

Verify DNS, browser trust, Secure cookies, WebSocket, and SSE against your own
ingress. The [synthetic TLS scenario](../examples/mini/README.md) uses a temporary
CA trusted only by its test client; it does not verify system or browser trust.
