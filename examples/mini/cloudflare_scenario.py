#!/usr/bin/env python3
"""Exercise live and snapshot sharing through a real Cloudflare Quick Tunnel."""
import base64
import hashlib
import http.client
import http.cookies
import json
import os
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from urllib.parse import urlencode, urlsplit

HERE = Path(__file__).resolve().parent
ENV = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
ENV.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
RDR = os.environ.get("RDR_BIN") or (sys.argv[1] if len(sys.argv) > 1 else "rdr")
RDR = shutil.which(RDR) or str(Path(RDR).expanduser().resolve())
CLOUDFLARED = os.environ.get("CLOUDFLARED_BIN") or shutil.which("cloudflared")


class ScenarioError(RuntimeError):
    pass


def cli(cwd, *args, ok=True):
    proc = subprocess.run([RDR, *args, "--json"], cwd=cwd, env=ENV, text=True,
                          capture_output=True, timeout=120)
    try:
        data = json.loads(proc.stdout)
    except json.JSONDecodeError as exc:
        raise ScenarioError(f"rdr {args[0]} returned invalid JSON (exit {proc.returncode})") from exc
    if (proc.returncode == 0) != ok:
        codes = ",".join(item.get("code", "UNKNOWN") for item in data.get("errors", []))
        logs = [path for item in data.get("results", []) for path in item.get("log_paths", [])]
        raise ScenarioError(f"rdr {args[0]} returned exit {proc.returncode}, errors={codes}, logs={logs}")
    return data


def git(cwd, *args):
    subprocess.run(["git", *args], cwd=cwd, env=ENV, check=True,
                   stdout=subprocess.DEVNULL, timeout=30)


def https(url, path="/", method="GET", headers=None, body=None, timeout=8):
    parsed = urlsplit(url)
    if parsed.scheme != "https" or not parsed.hostname:
        raise ScenarioError("share URL was not HTTPS")
    conn = http.client.HTTPSConnection(parsed.hostname, parsed.port or 443,
                                       context=ssl.create_default_context(), timeout=timeout)
    conn.request(method, path, body=body, headers=headers or {})
    response = conn.getresponse()
    payload = response.read(1 << 20)
    result = response.status, response.headers, payload
    conn.close()
    return result


def external(url, path="/", method="GET", headers=None, body=None, acceptable=None):
    last = None
    for attempt in range(12):
        try:
            result = https(url, path, method, headers, body)
            if acceptable is None or result[0] in acceptable:
                return result
            if result[0] not in (502, 503, 530, 522, 524):
                return result
            last = f"HTTP {result[0]}"
        except (OSError, http.client.HTTPException, ssl.SSLError) as exc:
            last = type(exc).__name__
        if attempt < 11:
            time.sleep(2)
    raise ScenarioError(f"Cloudflare endpoint {urlsplit(url).hostname} did not become ready: {last}")


def login(url, password, next_path):
    host = urlsplit(url).hostname
    body = urlencode({"password": password, "next": next_path})
    status, headers, _ = external(
        url, "/_rdr/login", "POST",
        {"Origin": "https://" + host, "Content-Type": "application/x-www-form-urlencoded"},
        body, {303})
    set_cookie = "; ".join(headers.get_all("Set-Cookie", []))
    if "Secure" not in set_cookie or "HttpOnly" not in set_cookie:
        raise ScenarioError("login did not set a Secure, HttpOnly session cookie")
    cookies = http.cookies.SimpleCookie()
    cookies.load(set_cookie)
    item = next((value for value in cookies.values() if value.key.startswith("__Host-rdr-")), None)
    if item is None or not item.key.startswith("__Host-rdr-"):
        raise ScenarioError("login did not set a host-scoped gateway cookie")
    return f"{item.key}={item.value}"


def websocket(url, cookie, path="/socket"):
    host = urlsplit(url).hostname
    context = ssl.create_default_context()
    raw = socket.create_connection((host, 443), timeout=10)
    tls = context.wrap_socket(raw, server_hostname=host)
    key = base64.b64encode(os.urandom(16)).decode()
    tls.sendall((f"GET {path} HTTP/1.1\r\nHost: {host}\r\nConnection: Upgrade\r\n"
                 f"Upgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: {key}\r\n"
                 f"Cookie: {cookie}\r\n\r\n").encode())
    reply = b""
    while b"\r\n\r\n" not in reply:
        chunk = tls.recv(4096)
        if not chunk:
            break
        reply += chunk
    head, _, frame = reply.partition(b"\r\n\r\n")
    while len(frame) < 12:
        chunk = tls.recv(64)
        if not chunk:
            break
        frame += chunk
    tls.close()
    lines = head.decode("latin1").split("\r\n")
    accept = next((line.split(":", 1)[1].strip() for line in lines[1:]
                   if line.split(":", 1)[0].lower() == "sec-websocket-accept"), "")
    expected = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
    if b" 101 " not in head.lower() or accept != expected or frame[:2] != b"\x81\x0a" or frame[2:12] != b"HMR:ready!":
        raise ScenarioError("authenticated WebSocket handshake or HMR frame failed")


def run_ids(cwd, project):
    return {item["instance_id"]: item.get("run_id")
            for item in cli(cwd, "status", "--project", project).get("results", [])}


def assert_unshared(url, path, cookie, expected_body):
    try:
        status, _, body = https(url, path, headers={"Cookie": cookie}, timeout=8)
    except (OSError, http.client.HTTPException, ssl.SSLError):
        return
    if status == 200 and body == expected_body:
        raise ScenarioError("unshared URL still served authenticated application content")


def main():
    if not CLOUDFLARED or not Path(CLOUDFLARED).is_file() or not os.access(CLOUDFLARED, os.X_OK):
        print(f"INCOMPLETE: installed cloudflared is not executable: {CLOUDFLARED}", file=sys.stderr)
        return 2
    temp = Path(tempfile.mkdtemp(prefix="rdr-mini-cloudflare-"))
    repo, runtime, tools = temp / "repo", temp / "runtime", temp / "tools"
    repo.mkdir()
    tools.mkdir()
    (tools / "cloudflared").symlink_to(Path(CLOUDFLARED).resolve())
    ENV.update(RDR_HOME=str(runtime), XDG_CONFIG_HOME=str(temp / "config"),
               XDG_STATE_HOME=str(temp / "state"),
               PATH=str(tools) + os.pathsep + ENV.get("PATH", os.defpath))
    (temp / "config").mkdir()
    (temp / "state").mkdir()
    cleanup_needed = False
    project = ""
    publication_url = ""
    problem = None
    try:
        shutil.copyfile(HERE / "service.py", repo / "service.py")
        shutil.copyfile(HERE / "build_snapshot.py", repo / "build_snapshot.py")
        (repo / "source-marker.txt").write_text("cloudflare-snapshot-A\n")
        dev = (HERE / "dev.toml").read_text()
        start = dev.index("[services.reports]")
        end = dev.index("[services.indexer]")
        reports = dev[start:end].replace('depends_on = ["catalog"]', 'depends_on = ["catalog"]\nroutes = [{ path = "/catalog", dependency = "catalog", strip_prefix = true }]')
        reports += '[services.reports.share]\nmode = "snapshot"\nbuild = "python3 build_snapshot.py"\ndir = "dist"\n'
        # Keep only the synthetic reports service and its catalog dependency.
        dev = dev[:start] + reports
        held = []
        try:
            for _ in range(2):
                listener = socket.socket()
                listener.bind(("127.0.0.1", 0))
                held.append(listener)
                dev = dev.replace('port = "auto"', f'port = {listener.getsockname()[1]}', 1)
            (repo / "dev.toml").write_text(dev)
        finally:
            for listener in held:
                listener.close()
        template = temp / "empty-template"
        template.mkdir()
        git(repo, "init", "--initial-branch=main", f"--template={template}")
        for name, value in (("core.hooksPath", "/dev/null"), ("core.fsmonitor", "false"),
                            ("commit.gpgSign", "false"), ("user.name", "rdr cloudflare fixture"),
                            ("user.email", "cloudflare-fixture@invalid")):
            git(repo, "config", name, value)
        git(repo, "add", "dev.toml", "service.py", "build_snapshot.py", "source-marker.txt")
        git(repo, "commit", "-m", "synthetic Cloudflare share acceptance fixture")
        cleanup_needed = True
        registration = cli(repo, "register", ".")
        project = registration["scope"]["project"]
        cli(repo, "up", "reports", "--project", project, "--worktree", str(repo))
        before = run_ids(repo, project)
        if not any(key.endswith("/reports") and value for key, value in before.items()):
            raise ScenarioError("reports fixture did not start")

        live = cli(repo, "share", "reports", "--project", project, "--worktree", str(repo), "--mode", "live")
        password = live.get("credentials", {}).get("password", "")
        publication_url = live.get("results", [{}])[0].get("url", "")
        if not password or not publication_url.startswith("https://") or json.dumps(live).count(password) != 1:
            raise ScenarioError("live share did not return one HTTPS URL and one-time password")
        for path, upgrade in (("/", False), ("/identity", False), ("/socket", True)):
            headers = {"Connection": "Upgrade", "Upgrade": "websocket"} if upgrade else None
            status, _, _ = external(publication_url, path, headers=headers, acceptable={303})
            if status != 303:
                raise ScenarioError(f"anonymous live request to {path} was not gated")
        cookie = login(publication_url, password, "/identity")
        status, _, body = external(publication_url, "/identity", headers={"Cookie": cookie}, acceptable={200})
        if status != 200 or json.loads(body).get("role") != "reports":
            raise ScenarioError("authenticated live API did not reach the synthetic reports service")
        live_body = body
        status, headers, _ = external(publication_url, "/session", headers={"Cookie": cookie}, acceptable={200})
        if status != 200 or not any("mini_session=secure-fixture" in value for value in headers.get_all("Set-Cookie", [])):
            raise ScenarioError("live share did not preserve the app Secure cookie")
        app_cookie = cookie + "; mini_session=secure-fixture"
        if json.loads(external(publication_url, "/session", headers={"Cookie": app_cookie}, acceptable={200})[2]).get("roundtrip") is not True:
            raise ScenarioError("live share app cookie did not roundtrip")
        websocket(publication_url, cookie)

        cli(repo, "unshare", "reports", "--project", project, "--worktree", str(repo))
        if run_ids(repo, project) != before:
            raise ScenarioError("live unshare changed local app RunIDs")
        assert_unshared(publication_url, "/identity", cookie, live_body)
        publication_url = ""

        snapshot = cli(repo, "share", "reports", "--project", project, "--worktree", str(repo), "--mode", "snapshot")
        snapshot_password = snapshot.get("credentials", {}).get("password", "")
        snapshot_url = snapshot.get("results", [{}])[0].get("url", "")
        if not snapshot_password or not snapshot_url.startswith("https://"):
            raise ScenarioError("snapshot share did not return HTTPS URL and one-time password")
        publication_url = snapshot_url
        if external(snapshot_url, "/", acceptable={303})[0] != 303:
            raise ScenarioError("anonymous snapshot root was not gated")
        snapshot_cookie = login(snapshot_url, snapshot_password, "/")

        def page():
            status, _, body = external(snapshot_url, "/", headers={"Cookie": snapshot_cookie}, acceptable={200})
            if status != 200:
                raise ScenarioError("authenticated snapshot request failed")
            return json.loads(body), body

        artifact, artifact_bytes = page()
        if artifact.get("marker") != "cloudflare-snapshot-A" or artifact.get("public_url") != snapshot_url or artifact.get("catalog_url") != snapshot_url + "/catalog":
            raise ScenarioError("snapshot artifact omitted source marker or public URL")
        status, _, api_body = external(snapshot_url, "/catalog/identity", headers={"Cookie": snapshot_cookie}, acceptable={200})
        if status != 200 or json.loads(api_body).get("role") != "catalog":
            raise ScenarioError("authenticated snapshot dependency route failed")
        websocket(snapshot_url, snapshot_cookie, path="/catalog/socket")
        (repo / "source-marker.txt").write_text("cloudflare-snapshot-B\n")
        shutil.rmtree(repo / "dist", ignore_errors=True)
        if page()[1] != artifact_bytes:
            raise ScenarioError("published snapshot changed after source edit/deletion")
        ids_before_unshare = run_ids(repo, project)
        cli(repo, "unshare", "reports", "--project", project, "--worktree", str(repo))
        assert_unshared(snapshot_url, "/", snapshot_cookie, artifact_bytes)
        publication_url = ""
        if run_ids(repo, project) != ids_before_unshare:
            raise ScenarioError("snapshot unshare changed local app RunIDs")
    except Exception as exc:
        problem = exc
    if cleanup_needed:
        try:
            scope = ["--project", project] if project else []
            public = cli(repo, "status", *scope)
            if public.get("publications"):
                if project:
                    cli(repo, "unshare", "reports", "--project", project, "--worktree", str(repo))
            cli(repo, "down", "--all")
            status = cli(repo, "status", *scope)
            bad = [item for item in status.get("results", []) if item.get("state_unknown") or
                   item.get("state") != "stopped" or item.get("preparation")]
            if status.get("status") != "ok" or bad or status.get("publications"):
                raise ScenarioError(f"cleanup not confirmed: states={len(bad)} publications={len(status.get('publications', []))}")
        except Exception as exc:
            print(f"CLEANUP FAILED: {type(exc).__name__}: {exc}", file=sys.stderr)
            print(f"Fixture and runtime retained at {temp}", file=sys.stderr)
            return 3
    if problem:
        print(f"INCOMPLETE: {type(problem).__name__}: {problem}", file=sys.stderr)
        print(f"Cleanup confirmed; synthetic fixture retained for diagnostics: {temp}", file=sys.stderr)
        return 2
    shutil.rmtree(temp)
    print("PASS: real Cloudflare HTTPS live/snapshot auth, cookies, WebSocket, immutable artifact and cleanup")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
