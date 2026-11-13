#!/usr/bin/env python3
"""Offline cloudflared stand-in for share_scenario.py."""
import http.server
import json
import os
import secrets
import select
import signal
import socket
import socketserver
import ssl
import sys
import threading
from pathlib import Path
from urllib.parse import urlsplit


def main():
    args = sys.argv[1:]
    try:
        origin = args[args.index("--url") + 1]
    except (ValueError, IndexError):
        raise SystemExit("fake cloudflared requires --url")
    fail_marker = os.environ.get("FAKE_CF_FAIL_NEXT", "")
    if fail_marker and Path(fail_marker).exists():
        Path(fail_marker).unlink()
        print("fake tunnel startup failure", file=sys.stderr, flush=True)
        return 7

    parsed_origin = urlsplit(origin)
    if parsed_origin.scheme != "http" or parsed_origin.hostname != "127.0.0.1" or not parsed_origin.port:
        raise SystemExit("fake cloudflared accepts only a loopback HTTP origin")
    origin_host, origin_port = parsed_origin.hostname, parsed_origin.port
    host = "mini-" + secrets.token_hex(8) + ".trycloudflare.com"
    map_dir = Path(os.environ["FAKE_CF_MAP"])
    map_dir.mkdir(parents=True, exist_ok=True)

    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *_):
            pass

        def do_GET(self):
            self.forward()

        def do_POST(self):
            self.forward()

        def forward(self):
            upgrade = self.headers.get("Upgrade", "").lower() == "websocket"
            try:
                upstream = socket.create_connection((origin_host, origin_port), timeout=5)
                headers = []
                for key, value in self.headers.items():
                    if key.lower() in {"x-forwarded-proto", "connection"}:
                        continue
                    headers.append((key, value))
                headers.append(("X-Forwarded-Proto", "https"))
                headers.append(("Connection", "Upgrade" if upgrade else "close"))
                request = f"{self.command} {self.path} HTTP/1.1\r\n"
                request += "".join(f"{key}: {value}\r\n" for key, value in headers)
                upstream.sendall((request + "\r\n").encode("latin1"))
                length = int(self.headers.get("Content-Length", "0"))
                remaining = length
                while remaining:
                    chunk = self.rfile.read(min(65536, remaining))
                    if not chunk:
                        break
                    upstream.sendall(chunk)
                    remaining -= len(chunk)
                if upgrade:
                    self.bridge(upstream)
                else:
                    while True:
                        chunk = upstream.recv(65536)
                        if not chunk:
                            break
                        self.connection.sendall(chunk)
                upstream.close()
            except (OSError, ValueError):
                try:
                    self.send_error(502)
                except OSError:
                    pass

        def bridge(self, upstream):
            while True:
                ready, _, _ = select.select([self.connection, upstream], [], [], 10)
                if not ready:
                    return
                for source in ready:
                    try:
                        payload = source.recv(65536)
                    except OSError:
                        return
                    if not payload:
                        return
                    target = upstream if source is self.connection else self.connection
                    try:
                        target.sendall(payload)
                    except OSError:
                        return

    class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
        daemon_threads = True
        allow_reuse_address = True

    server = Server(("127.0.0.1", 0), Handler)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(os.environ["FAKE_CF_CERT"], os.environ["FAKE_CF_KEY"])
    server.socket = context.wrap_socket(server.socket, server_side=True)
    map_path = map_dir / (host + ".json")
    map_path.write_text(json.dumps({"host": host, "port": server.server_port}))
    print(f"Tunnel URL https://{host}", flush=True)
    print("Registered tunnel connection", flush=True)
    signal.signal(signal.SIGTERM, lambda *_: threading.Thread(target=server.shutdown, daemon=True).start())
    try:
        server.serve_forever(poll_interval=0.2)
    finally:
        map_path.unlink(missing_ok=True)
        server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
