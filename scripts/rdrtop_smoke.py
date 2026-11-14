#!/usr/bin/env python3
"""Exercise rdrtop in a real PTY, including stop hooks and terminal restoration."""

import fcntl
import http.client
import json
import os
from pathlib import Path
import pty
import select
import signal
import struct
import subprocess
import tempfile
import termios
import time
from urllib.parse import urlsplit


def wait_for(fd, needle, process, timeout=10):
    captured = b""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if select.select([fd], [], [], 0.1)[0]:
            captured += os.read(fd, 65536)
            if needle in captured:
                return captured
        if process.poll() is not None:
            break
    raise AssertionError(f"missing {needle!r} in PTY: {captured[-3000:]!r}")


def main():
    repo = Path(__file__).resolve().parents[1]
    with tempfile.TemporaryDirectory(prefix="rdrtop-smoke-") as temporary:
        root = Path(temporary).resolve()
        rdr, top = root / "rdr", root / "rdrtop"
        for command, destination in [("rdr", rdr), ("rdrtop", top)]:
            subprocess.run(["go", "build", "-o", str(destination), f"./cmd/{command}"], cwd=repo, check=True)
        project = root / "project"
        project.mkdir()
        (project / "dev.toml").write_text('''version = 1
[project]
name = "pty-fixture"
[services.worker]
command = "echo pty-service-ready; exec sleep 60"
stop_timeout = "3s"
hooks = { before_stop = "printf done > before-stopped", after_stop = "printf done > after-stopped" }
''')
        env = dict(os.environ, RDR_HOME=str(root / "state"), XDG_CONFIG_HOME=str(root / "config"))

        def cli(*args, cwd=project, check=True):
            result = subprocess.run([str(rdr), *args, "--json"], cwd=cwd, env=env,
                                    capture_output=True, text=True, timeout=15)
            if check:
                assert result.returncode == 0, result.stdout + result.stderr
            return json.loads(result.stdout)

        def start_tui(cwd=project, needle=b"worker"):
            master, slave = pty.openpty()
            fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 100, 0, 0))
            original = termios.tcgetattr(slave)
            process = subprocess.Popen([str(top)], cwd=cwd, env=env,
                                       stdin=slave, stdout=slave, stderr=slave)
            wait_for(master, needle, process)
            return process, master, slave, original

        def close_tui(process, master, slave, original, key):
            try:
                os.write(master, key)
                # Keep consuming the final frame and alternate-screen cleanup;
                # a PTY can otherwise fill while waiting for the renderer to exit.
                deadline = time.monotonic() + 10
                while process.poll() is None and time.monotonic() < deadline:
                    if select.select([master], [], [], 0.1)[0]:
                        os.read(master, 65536)
                assert process.wait(timeout=10) == 0
                restored = termios.tcgetattr(slave)
                # PENDIN is kernel pending-input state, not a terminal setting.
                # A cancelable reader may leave it set when canonical mode returns.
                pending = getattr(termios, "PENDIN", 0)
                restored[3] &= ~pending
                original[3] &= ~pending
                assert restored == original, f"terminal attributes were not restored: {original!r} -> {restored!r}"
            finally:
                if process.poll() is None:
                    process.terminate()
                    process.wait(timeout=5)
                os.close(master)
                os.close(slave)

        try:
            cli("up", "worker", "--no-proxy")
            # Leaving the log view with Ctrl-C must restore the TTY and keep the service alive.
            process, master, slave, original = start_tui()
            try:
                # The networking view is available without blocking service controls.
                os.write(master, b"n")
                wait_for(master, b"Local HTTP", process)
                os.write(master, b"\x1b")
                wait_for(master, b"worker", process)
                os.write(master, b"l")
                wait_for(master, b"pty-service-ready", process)
            finally:
                close_tui(process, master, slave, original, b"\x03")
            status = cli("status")
            assert status["results"][0]["state"] in {"running", "ready"}, status
            assert not (project / "before-stopped").exists()
            # Stop must execute both hooks through rdrtop's bootstrap entrypoint.
            process, master, slave, original = start_tui()
            try:
                fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 14, 60, 0, 0))
                process.send_signal(signal.SIGWINCH)
                os.write(master, b"s")
                wait_for(master, "Process stopped".encode(), process)
                assert (project / "before-stopped").read_text() == "done"
                assert (project / "after-stopped").read_text() == "done"
            finally:
                close_tui(process, master, slave, original, b"q")
            assert cli("status")["results"][0]["state"] == "stopped"
            # A4: launch the actual rdrtop binary with a live HTTP service and
            # no proxy process. Its own __rdr_proxy entrypoint must restore local access.
            web = root / "http-project"
            web.mkdir()
            (web / "server.py").write_text('''import http.server, os
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = ("local-route-ok:" + self.headers.get("Host", "")).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *_): pass
http.server.HTTPServer(("127.0.0.1", int(os.environ["RDR_PORT"])), Handler).serve_forever()
''')
            (web / "dev.toml").write_text('''version = 1
[project]
name = "http-fixture"
[services.web]
command = "exec python3 -u server.py"
protocol = "http"
port = "auto"
stop_timeout = "3s"
''')
            web_up = cli("up", "web", cwd=web)
            assert web_up["status"] == "ok", web_up
            before = next(result for result in cli("status", cwd=web)["results"] if result["service"] == "web")
            run_id = before["run_id"]
            proxy_stopped = cli("proxy", "stop", cwd=web)
            assert proxy_stopped["status"] == "ok", proxy_stopped
            stopped = cli("proxy", "status", cwd=web)
            assert stopped.get("proxy", {}).get("state") == "stopped", stopped

            process, master, slave, original = start_tui(web, b"web")
            try:
                wait_for(master, b"web", process)
                route = None
                deadline = time.monotonic() + 15
                while time.monotonic() < deadline:
                    current = cli("status", cwd=web)
                    candidate = next(result for result in current["results"] if result["service"] == "web")
                    route = candidate.get("url")
                    if route and ".localhost" in route:
                        break
                    time.sleep(0.1)
                assert route and ".localhost" in route, f"missing canonical local URL: {candidate}"
                parsed = urlsplit(route)
                assert parsed.scheme == "http" and parsed.port == 8080, route
                response = None
                deadline = time.monotonic() + 15
                while time.monotonic() < deadline:
                    try:
                        connection = http.client.HTTPConnection("127.0.0.1", parsed.port, timeout=2)
                        connection.request("GET", parsed.path or "/", headers={"Host": parsed.hostname})
                        response = connection.getresponse()
                        payload = response.read().decode()
                        connection.close()
                        if response.status == 200 and payload == "local-route-ok:" + parsed.hostname:
                            break
                    except OSError:
                        pass
                    time.sleep(0.1)
                assert response and response.status == 200 and payload == "local-route-ok:" + parsed.hostname, payload if response else "no local response"
            finally:
                close_tui(process, master, slave, original, b"q")

            after = next(result for result in cli("status", cwd=web)["results"] if result["service"] == "web")
            assert after["run_id"] == run_id, (run_id, after)
            assert after["state"] in {"running", "ready"}, after
            direct = urlsplit(after["direct_url"])
            connection = http.client.HTTPConnection(direct.hostname, direct.port, timeout=2)
            connection.request("GET", direct.path or "/")
            direct_response = connection.getresponse()
            direct_payload = direct_response.read().decode()
            connection.close()
            assert direct_response.status == 200 and direct_payload.startswith("local-route-ok:"), direct_payload
            connection = http.client.HTTPConnection("127.0.0.1", parsed.port, timeout=2)
            connection.request("GET", parsed.path or "/", headers={"Host": parsed.hostname})
            routed_response = connection.getresponse()
            routed_payload = routed_response.read().decode()
            connection.close()
            assert routed_response.status == 200 and routed_payload == "local-route-ok:" + parsed.hostname, routed_payload
            proxy_after = cli("proxy", "status", cwd=web)
            assert proxy_after.get("proxy", {}).get("state") == "ready", proxy_after
            print("PASS: PTY navigation/logs, terminal restoration, hooks, rdrtop-owned HTTP proxy startup, route Host, and process persistence")
        finally:
            cli("down", "--all", check=False)
            cli("proxy", "stop", check=False)


if __name__ == "__main__":
    main()
