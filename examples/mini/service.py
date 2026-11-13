#!/usr/bin/env python3
"""Small stdlib-only services for the rundevrun multi-worktree example."""
import json
import os
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.request import urlopen


def identity(role):
    return {
        "role": role,
        "worktree": os.environ.get("WORKTREE", "unknown"),
        "project": os.environ.get("PROJECT", "unknown"),
        "service": os.environ.get("ROLE", role),
    }


def request_json(url):
    with urlopen(url, timeout=3) as response:
        return json.load(response)


class Handler(BaseHTTPRequestHandler):
    role = ""

    def do_GET(self):
        if self.path == "/health":
            self.reply({"ok": True})
        elif self.path == "/identity":
            data = identity(self.role)
            if self.role == "reports":
                data["catalog"] = request_json(os.environ["CATALOG_URL"] + "/identity")
            self.reply(data)
        else:
            self.send_error(404)

    def reply(self, data):
        body = json.dumps(data, sort_keys=True).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        print("http: " + fmt % args, flush=True)


def serve(role):
    Handler.role = role
    server = ThreadingHTTPServer((os.environ.get("RDR_HOST", "127.0.0.1"), int(os.environ["RDR_PORT"])), Handler)
    print(json.dumps({"event": "listening", **identity(role), "port": server.server_port}), flush=True)
    server.serve_forever()


def index():
    url = os.environ["CATALOG_URL"]
    interval = float(os.environ.get("POLL_SECONDS", "2"))
    while True:
        try:
            print(json.dumps({"event": "indexed", "catalog": request_json(url + "/identity")}, sort_keys=True), flush=True)
        except Exception as exc:
            print(json.dumps({"event": "catalog_error", "error": str(exc)}), flush=True)
        time.sleep(interval)


if __name__ == "__main__":
    if len(sys.argv) != 2 or sys.argv[1] not in {"catalog", "reports", "indexer"}:
        raise SystemExit("usage: service.py catalog|reports|indexer")
    serve(sys.argv[1]) if sys.argv[1] in {"catalog", "reports"} else index()
