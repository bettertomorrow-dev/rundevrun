#!/usr/bin/env python3
"""Public-CLI TLS/proxy acceptance fixture for the synthetic mini services."""
import http.client
import json
import base64
import hashlib
import os
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from urllib.parse import urlsplit

HERE = Path(__file__).resolve().parent
ENV = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
ENV.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
RDR = os.environ.get("RDR_BIN") or (sys.argv[1] if len(sys.argv) > 1 else "rdr")
RDR = shutil.which(RDR) or str(Path(RDR).expanduser().resolve())


class Incomplete(RuntimeError):
    pass


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def cli(cwd, *args, ok=True):
    p = subprocess.run([RDR, *args, "--json"], cwd=cwd, env=ENV, text=True,
                       capture_output=True, timeout=45)
    try:
        result = json.loads(p.stdout)
    except json.JSONDecodeError as exc:
        raise Incomplete(f"rdr {args[0]} returned invalid JSON: {p.stderr.strip()}") from exc
    if (p.returncode == 0) != ok:
        raise Incomplete(f"rdr {' '.join(args)} exit {p.returncode}: {result.get('errors')}")
    return result


def git(cwd, *args):
    subprocess.run(["git", *args], cwd=cwd, env=ENV, check=True,
                   stdout=subprocess.DEVNULL, timeout=30)


def make_certificates(temp):
    ca_key, ca_cert = temp / "ca.key", temp / "ca.crt"
    key, csr, cert, ext = temp / "leaf.key", temp / "leaf.csr", temp / "leaf.crt", temp / "leaf.ext"
    def openssl(*args):
        subprocess.run(["openssl", *args], check=True, stdout=subprocess.DEVNULL,
                       stderr=subprocess.PIPE, timeout=20)
    openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", str(ca_key),
            "-out", str(ca_cert), "-days", "2", "-subj", "/CN=mini-fixture-ca",
            "-addext", "basicConstraints=critical,CA:TRUE")
    openssl("req", "-newkey", "rsa:2048", "-nodes", "-keyout", str(key), "-out", str(csr),
            "-subj", "/CN=*.mini.test")
    ext.write_text("subjectAltName=DNS:*.mini.test,DNS:mini.test\nextendedKeyUsage=serverAuth\n")
    openssl("x509", "-req", "-in", str(csr), "-CA", str(ca_cert), "-CAkey", str(ca_key),
            "-CAcreateserial", "-out", str(cert), "-days", "2", "-extfile", str(ext))
    return ca_cert, cert, key


def https(url, context, method="GET", path="/", headers=None, body=None):
    parsed = urlsplit(url)
    class LocalTLS(http.client.HTTPSConnection):
        def connect(self):
            raw = socket.create_connection(("127.0.0.1", parsed.port), timeout=5)
            self.sock = context.wrap_socket(raw, server_hostname=parsed.hostname)
    conn = LocalTLS(parsed.hostname, parsed.port, context=context, timeout=5)
    conn.request(method, path, body=body, headers=headers or {})
    response = conn.getresponse()
    data = response.read(16384)
    status, hdrs = response.status, response.headers
    conn.close()
    return status, hdrs, data


def websocket(url, context):
    parsed = urlsplit(url)
    raw = socket.create_connection(("127.0.0.1", parsed.port), timeout=4)
    tls = context.wrap_socket(raw, server_hostname=parsed.hostname)
    key = base64.b64encode(os.urandom(16)).decode()
    tls.sendall((f"GET /socket HTTP/1.1\r\nHost: {parsed.hostname}\r\n"
                 "Connection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\n"
                 f"Sec-WebSocket-Key: {key}\r\n\r\n").encode())
    reply = b""
    while b"\r\n\r\n" not in reply:
        part = tls.recv(4096)
        if not part:
            break
        reply += part
    head, _, rest = reply.partition(b"\r\n\r\n")
    header_lines = head.decode("latin1").split("\r\n")[1:]
    accept = next((line.split(":", 1)[1].strip() for line in header_lines
                   if line.split(":", 1)[0].lower() == "sec-websocket-accept"), "")
    frame = rest
    while len(frame) < 2 + 10:
        part = tls.recv(64)
        if not part:
            break
        frame += part
    tls.close()
    expected = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
    if b" 101 " not in head.lower() or accept != expected or frame[:2] != b"\x81\x0a" or frame[2:12] != b"HMR:ready!":
        raise Incomplete(f"WebSocket handshake or HMR frame failed: {head!r} {frame!r}")


def status_instances(cwd, project):
    result = cli(cwd, "status", "--project", project)
    return {x["service"] + "@" + x["worktree_id"]: x for x in result["results"]}


def request_identity(url, context, path="/identity"):
    code, _, body = https(url, context, path=path)
    if code != 200:
        raise Incomplete(f"identity route returned HTTP {code}")
    return json.loads(body)


def main():
    temp = Path(tempfile.mkdtemp(prefix="rdr-mini-tls-"))
    repo, runtime = temp / "repo", temp / "runtime"
    ENV["RDR_HOME"] = str(runtime)
    ENV["XDG_CONFIG_HOME"] = str(temp / "config")
    (temp / "config" / "rundevrun").mkdir(parents=True)
    roots = {"main": repo, "a": temp / "wt-a"}
    registered = False
    project = ""
    clean = False
    try:
        repo.mkdir()
        shutil.copyfile(HERE / "dev.toml", repo / "dev.toml")
        shutil.copyfile(HERE / "service.py", repo / "service.py")
        template = temp / "empty-template"
        template.mkdir()
        git(repo, "init", "--initial-branch=main", f"--template={template}")
        for key, value in (("core.hooksPath", "/dev/null"), ("core.fsmonitor", "false"),
                           ("commit.gpgSign", "false"), ("user.name", "rdr fixture"),
                           ("user.email", "fixture@invalid")):
            git(repo, "config", key, value)
        git(repo, "add", "dev.toml", "service.py")
        git(repo, "commit", "-m", "synthetic TLS mini fixture")
        git(repo, "worktree", "add", "-b", "tls-a", str(roots["a"]))

        result = cli(repo, "register", ".")
        registered, project = True, result["scope"]["project"]
        discovered = cli(repo, "worktrees", "--project", project)["results"]
        known = {Path(x["root"]).resolve() for x in discovered}
        if known != {p.resolve() for p in roots.values()}:
            raise Incomplete("worktrees JSON did not list both fixture roots")

        ca, cert, key = make_certificates(temp)
        cfgdir = temp / "config" / "rundevrun"
        cfgdir.mkdir(parents=True, exist_ok=True)
        config = cfgdir / "config.toml"
        http_port, https_port = free_port(), free_port()
        config.write_text(
            "version=1\n[routing]\nenabled=true\ndomain='mini.test'\n"
            f"http_listen='127.0.0.1:{http_port}'\nhttps_listen='127.0.0.1:{https_port}'\n"
            f"[[routing.tls.certificates]]\ncert_file='{cert}'\nkey_file='{key}'\n"
        )

        for root in roots.values():
            dev = root / "dev.toml"
            text = dev.read_text().replace(
                'depends_on = ["catalog"]\nhealth = { http = "/health" }',
                'depends_on = ["catalog"]\nroutes = [{ path = "/catalog", dependency = "catalog", strip_prefix = true }]\nhealth = { http = "/health" }',
            )
            dev.write_text(text)
        cli(roots["a"], "bind", "reports", "catalog", "--project", project,
            "--worktree", str(roots["a"]), "--to-worktree", str(repo))
        for root in roots.values():
            cli(root, "config", "check", "--project", project, "--worktree", str(root))

        cli(repo, "up", "catalog", "reports", "--project", project, "--worktree", str(repo))
        cli(roots["a"], "up", "catalog", "reports", "--project", project, "--worktree", str(roots["a"]))
        context = ssl.create_default_context(cafile=str(ca))
        instances = status_instances(repo, project)
        byroot = {Path(x["root"]).resolve(): x for x in cli(repo, "worktrees", "--project", project)["results"]}
        main_id, a_id = byroot[repo.resolve()]["id"], byroot[roots["a"].resolve()]["id"]
        main_reports = instances[f"reports@{main_id}"]
        a_reports = instances[f"reports@{a_id}"]
        main_catalog = instances[f"catalog@{main_id}"]
        if Path(request_identity(main_reports["url"], context)["catalog"]["worktree"]).resolve() != roots["main"].resolve():
            raise Incomplete("reports in main did not reach its selected catalog")
        if Path(request_identity(a_reports["url"], context)["catalog"]["worktree"]).resolve() != roots["main"].resolve():
            raise Incomplete("reports direct dependency in A did not reach its shared main catalog")
        routed_catalog = request_identity(a_reports["url"], context, "/catalog/identity")
        if Path(routed_catalog["worktree"]).resolve() != roots["main"].resolve():
            raise Incomplete("HTTPS dependency route did not follow A's shared main catalog binding")


        code, headers, body = https(main_reports["url"], context, path="/session")
        if code != 200 or "Secure" not in headers.get("Set-Cookie", ""):
            raise Incomplete("HTTPS route did not set the secure fixture cookie")
        code, _, body = https(main_reports["url"], context, path="/session",
                              headers={"Cookie": "mini_session=secure-fixture"})
        if code != 200 or json.loads(body).get("roundtrip") is not True:
            raise Incomplete("secure cookie roundtrip failed")
        sse_url = urlsplit(main_reports["url"])
        sse_conn = http.client.HTTPSConnection(sse_url.hostname, sse_url.port, context=context, timeout=4)
        def local_sse_connect():
            raw = socket.create_connection(("127.0.0.1", sse_url.port), timeout=4)
            sse_conn.sock = context.wrap_socket(raw, server_hostname=sse_url.hostname)
        sse_conn.connect = local_sse_connect
        sse_started = time.monotonic()
        sse_conn.request("GET", "/events", headers={"Host": sse_url.hostname})
        sse_response = sse_conn.getresponse()
        def read_sse_event():
            data = bytearray()
            while not data.endswith(b"\n\n"):
                part = sse_response.read1(1)
                if not part:
                    break
                data.extend(part)
            return bytes(data)
        first_event = read_sse_event()
        first_elapsed = time.monotonic() - sse_started
        second_event = read_sse_event()
        second_elapsed = time.monotonic() - sse_started
        sse_conn.close()
        if sse_response.status != 200 or "text/event-stream" not in sse_response.headers.get("Content-Type", "") or b"synthetic-mini-first" not in first_event or first_elapsed > 0.8 or b"synthetic-mini-second" not in second_event or second_elapsed < 0.8:
            raise Incomplete("SSE did not flush the first event and stream the delayed second event")
        websocket(main_reports["url"], context)


        before = status_instances(repo, project)
        collision = "collision.mini.test"
        cli(repo, "alias", "reports", collision, "--project", project, "--worktree", str(repo))
        cli(repo, "alias", "catalog", collision, "--project", project, "--worktree", str(repo), ok=False)
        after = status_instances(repo, project)
        if after[f"reports@{main_id}"].get("url") != f"https://{collision}:{https_port}":
            raise Incomplete("first route alias was not retained after collision")
        if after[f"catalog@{main_id}"].get("url") != before[f"catalog@{main_id}"].get("url"):
            raise Incomplete("alias collision modified the other route")
        main_reports = after[f"reports@{main_id}"]


        cli(roots["a"], "down", "reports", "--project", project, "--worktree", str(roots["a"]))
        unavailable = https(a_reports["url"], context)[0]
        if unavailable != 503:
            raise Incomplete(f"stopped route returned {unavailable}, expected 503")
        if Path(request_identity(main_reports["url"], context)["catalog"]["worktree"]).resolve() != roots["main"].resolve():
            raise Incomplete("stopped A route crossed into main worktree")

        ids_before = {key: x.get("run_id") for key, x in status_instances(repo, project).items()}
        cli(repo, "proxy", "stop")
        cli(repo, "up", "catalog", "--project", project, "--worktree", str(repo))
        if {key: x.get("run_id") for key, x in status_instances(repo, project).items()} != ids_before:
            raise Incomplete("proxy restart changed application run IDs")
        good_config = config.read_text()
        config.write_text(good_config.replace(str(cert), str(temp / "missing-cert.pem")))
        cli(repo, "proxy", "reload", ok=False)
        if Path(request_identity(main_reports["url"], context)["catalog"]["worktree"]).resolve() != roots["main"].resolve():
            raise Incomplete("invalid certificate reload disrupted the existing proxy")
        if {key: x.get("run_id") for key, x in status_instances(repo, project).items()} != ids_before:
            raise Incomplete("invalid certificate reload changed application run IDs")
        config.write_text(good_config)
        cli(repo, "proxy", "reload")
        clean = True
    except Exception as exc:
        print(f"INCOMPLETE: {exc}", file=sys.stderr)
    finally:
        if registered:
            try:
                cli(repo, "down", "--all")
                data = cli(repo, "status", "--project", project)
                bad = [x for x in data.get("results", []) if x.get("state_unknown") or
                       x.get("state") != "stopped" or x.get("preparation")]
                if data.get("status") != "ok" or bad:
                    raise Incomplete(f"status did not confirm stopped instances: {bad}")
                proxy_state = cli(repo, "proxy", "status").get("proxy") or {}
                if proxy_state.get("state") == "ready":
                    cli(repo, "proxy", "stop")
            except Exception as exc:
                print(f"CLEANUP FAILED: {exc}", file=sys.stderr)
                print(f"Fixture and runtime retained at {temp}", file=sys.stderr)
                raise SystemExit(3) from exc
        shutil.rmtree(temp)
    if clean:
        print("PASS: public HTTPS routing, cookie, SSE, WebSocket, dependency isolation, collision and reload checks; cleanup confirmed")
        return 0
    return 2


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as exc:
        print(f"INCOMPLETE: fixture setup failed: {exc}", file=sys.stderr)
        raise SystemExit(2)
