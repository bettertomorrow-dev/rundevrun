#!/usr/bin/env python3
"""Exercise live share with an offline loopback-only cloudflared stand-in."""
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
from pathlib import Path
from urllib.parse import urlencode, urlsplit

HERE = Path(__file__).resolve().parent
ENV = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
ENV.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
RDR = os.environ.get("RDR_BIN") or (sys.argv[1] if len(sys.argv) > 1 else "rdr")
RDR = shutil.which(RDR) or str(Path(RDR).expanduser().resolve())


class ScenarioError(RuntimeError):
    pass


def cli(cwd, *args, ok=True):
    proc = subprocess.run([RDR, *args, "--json"], cwd=cwd, env=ENV, text=True,
                          capture_output=True, timeout=50)
    try:
        data = json.loads(proc.stdout)
    except json.JSONDecodeError as exc:
        raise ScenarioError(f"rdr {args[0]} returned invalid JSON: {proc.stderr.strip()}") from exc
    if (proc.returncode == 0) != ok:
        errors = "; ".join(item.get("message", "") for item in data.get("errors", []))
        raise ScenarioError(f"rdr {' '.join(args)} exited {proc.returncode}: {errors}")
    return data


def git(cwd, *args):
    subprocess.run(["git", *args], cwd=cwd, env=ENV, check=True,
                   stdout=subprocess.DEVNULL, timeout=30)


def certificates(directory):
    ca_key, ca_cert = directory / "ca.key", directory / "ca.pem"
    leaf_key, csr, leaf_cert = directory / "fake.key", directory / "fake.csr", directory / "fake.pem"
    extensions = directory / "fake.ext"

    def openssl(*args):
        subprocess.run(["openssl", *args], check=True, stdout=subprocess.DEVNULL,
                       stderr=subprocess.PIPE, timeout=25)

    openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-keyout", str(ca_key),
            "-out", str(ca_cert), "-days", "2", "-subj", "/CN=rdr-offline-share-ca",
            "-addext", "basicConstraints=critical,CA:TRUE")
    openssl("req", "-newkey", "rsa:2048", "-nodes", "-keyout", str(leaf_key), "-out", str(csr),
            "-subj", "/CN=*.trycloudflare.com")
    extensions.write_text("subjectAltName=DNS:*.trycloudflare.com\nextendedKeyUsage=serverAuth\n")
    openssl("x509", "-req", "-in", str(csr), "-CA", str(ca_cert), "-CAkey", str(ca_key),
            "-CAcreateserial", "-out", str(leaf_cert), "-days", "2", "-extfile", str(extensions))
    return ca_cert, leaf_cert, leaf_key


def endpoint(url, mapping_dir):
    host = urlsplit(url).hostname
    try:
        mapping = json.loads((mapping_dir / (host + ".json")).read_text())
    except (OSError, json.JSONDecodeError) as exc:
        raise ScenarioError(f"fake provider has no loopback mapping for {host}") from exc
    if mapping.get("host") != host or not isinstance(mapping.get("port"), int):
        raise ScenarioError("fake provider mapping is invalid")
    return host, mapping["port"]


def request(url, ca_file, mapping_dir, path="/", method="GET", headers=None, body=None):
    host, port = endpoint(url, mapping_dir)
    context = ssl.create_default_context(cafile=str(ca_file))

    class LocalTLS(http.client.HTTPSConnection):
        def connect(self):
            raw = socket.create_connection(("127.0.0.1", port), timeout=5)
            self.sock = context.wrap_socket(raw, server_hostname=host)

    conn = LocalTLS(host, port, context=context, timeout=5)
    request_headers = {"Host": host}
    request_headers.update(headers or {})
    conn.request(method, path, body=body, headers=request_headers)
    response = conn.getresponse()
    payload = response.read(65536)
    status, response_headers = response.status, response.headers
    conn.close()
    return status, response_headers, payload


def websocket(url, ca_file, mapping_dir, cookie, path="/socket"):
    host, port = endpoint(url, mapping_dir)
    context = ssl.create_default_context(cafile=str(ca_file))
    raw = socket.create_connection(("127.0.0.1", port), timeout=5)
    tls = context.wrap_socket(raw, server_hostname=host)
    key = base64.b64encode(os.urandom(16)).decode()
    headers = [f"GET {path} HTTP/1.1", f"Host: {host}", "Connection: Upgrade",
               "Upgrade: websocket", "Sec-WebSocket-Version: 13", f"Sec-WebSocket-Key: {key}",
               f"Cookie: {cookie}", "", ""]
    tls.sendall("\r\n".join(headers).encode())
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
        raise ScenarioError(f"authenticated WebSocket failed: {head!r} {frame!r}")


def one(items, service):
    matches = [item for item in items if item.get("service") == service]
    if len(matches) != 1:
        raise ScenarioError(f"expected one {service} status entry, got {len(matches)}")
    return matches[0]


def publications(cwd, project):
    data = cli(cwd, "status", "--project", project)
    return data.get("publications", [])


def run_ids(cwd, project):
    return {item["instance_id"]: item.get("run_id")
            for item in cli(cwd, "status", "--project", project)["results"]}


def check_publication_output(cwd, project, password):
    for cmd in ("status", "logs"):
        if cmd == "status":
            data = cli(cwd, cmd, "--project", project)
        else:
            data = cli(cwd, cmd, "reports", "--project", project, "--worktree", str(cwd), "--tail", "30")
        encoded = json.dumps(data, sort_keys=True)
        for secret in (password, "signing_key", "verifier", "salt", "cloudflared.yml"):
            if secret in encoded:
                raise ScenarioError(f"{cmd} leaked publication secret field or value {secret!r}")


def main():
    temp = Path(tempfile.mkdtemp(prefix="rdr-mini-share-"))
    repo, runtime = temp / "repo", temp / "runtime"
    provider, mappings = temp / "provider", temp / "provider" / "mappings"
    repo.mkdir()
    provider.mkdir()
    mappings.mkdir()
    ENV["RDR_HOME"] = str(runtime)
    ENV["FAKE_CF_MAP"] = str(mappings)
    ENV["FAKE_CF_FAIL_NEXT"] = str(provider / "fail-next")
    credentials_dir = temp / "certificates"
    credentials_dir.mkdir()
    ca_cert, cert, key = certificates(credentials_dir)
    ENV["FAKE_CF_CERT"] = str(cert)
    ENV["FAKE_CF_KEY"] = str(key)
    fake_binary = provider / "cloudflared"
    shutil.copyfile(HERE / "fake_cf.py", fake_binary)
    fake_binary.chmod(0o700)
    ENV["PATH"] = str(provider) + os.pathsep + ENV.get("PATH", os.defpath)
    config_home = temp / "config"
    ENV["XDG_CONFIG_HOME"] = str(config_home)
    config_home.mkdir()
    registered = False
    project = ""
    passed = False
    try:
        shutil.copyfile(HERE / "service.py", repo / "service.py")
        dev = (HERE / "dev.toml").read_text().replace(
            'depends_on = ["catalog"]\nhealth = { http = "/health" }',
            'depends_on = ["catalog"]\nroutes = [{ path = "/catalog", dependency = "catalog", strip_prefix = true }]\nhealth = { http = "/health" }',
        )
        dev += '\n[services.reports.share]\nmode = "snapshot"\nbuild = "python3 build_snapshot.py"\ndir = "dist"\nenv = { SHARE_ONLY = "synthetic-share-override" }\n'
        (repo / "dev.toml").write_text(dev)
        shutil.copyfile(HERE / "build_snapshot.py", repo / "build_snapshot.py")
        (repo / "source-marker.txt").write_text("snapshot-source-A\n")
        template = temp / "empty-template"
        template.mkdir()
        git(repo, "init", "--initial-branch=main", f"--template={template}")
        for name, value in (("core.hooksPath", "/dev/null"), ("core.fsmonitor", "false"),
                            ("commit.gpgSign", "false"), ("user.name", "rdr offline share"),
                            ("user.email", "offline-share@invalid")):
            git(repo, "config", name, value)
        git(repo, "add", "dev.toml", "service.py", "build_snapshot.py", "source-marker.txt")
        git(repo, "commit", "-m", "synthetic live share fixture")
        registration = cli(repo, "register", ".")
        project, registered = registration["scope"]["project"], True
        cli(repo, "up", "reports", "--project", project, "--worktree", str(repo))
        initial_ids = run_ids(repo, project)
        if len(initial_ids) != 3 or not any(key.endswith("/reports") and run for key, run in initial_ids.items()) or not any(key.endswith("/catalog") and run for key, run in initial_ids.items()):
            raise ScenarioError(f"expected reports and catalog to be running: {initial_ids}")

        created = cli(repo, "share", "reports", "--project", project, "--worktree", str(repo), "--mode", "live")
        password = created.get("credentials", {}).get("password", "")
        if not password or len(created.get("results", [])) != 1:
            raise ScenarioError("new share did not return its one-time password")
        first = created["results"][0]
        first_url = first.get("url", "")
        if not first_url.startswith("https://"):
            raise ScenarioError("new share did not return HTTPS URL")
        if json.dumps(created).count(password) != 1:
            raise ScenarioError("new share password was not exposed exactly once in the JSON envelope")
        host, _ = endpoint(first_url, mappings)
        origin = "https://" + host

        code, headers, _ = request(first_url, ca_cert, mappings, "/identity")
        if code != 303 or not headers.get("Location", "").startswith("/_rdr/login"):
            raise ScenarioError("anonymous API request was not sent to the password gate")
        code, _, _ = request(first_url, ca_cert, mappings, "/")
        if code != 303:
            raise ScenarioError("anonymous static/root request bypassed the password gate")
        code, _, _ = request(first_url, ca_cert, mappings, "/catalog/identity")
        if code != 303:
            raise ScenarioError("anonymous dependency route bypassed the password gate")
        code, _, _ = request(first_url, ca_cert, mappings, "/socket", headers={"Connection": "Upgrade", "Upgrade": "websocket"})
        if code != 303:
            raise ScenarioError("anonymous WebSocket bypassed the password gate")

        form = urlencode({"password": password, "next": "/identity"})
        code, login_headers, _ = request(first_url, ca_cert, mappings, "/_rdr/login", "POST",
                                         {"Origin": origin, "Content-Type": "application/x-www-form-urlencoded"}, form)
        if code != 303 or "Secure" not in login_headers.get("Set-Cookie", "") or "HttpOnly" not in login_headers.get("Set-Cookie", ""):
            raise ScenarioError("password login did not set a secure session cookie")
        cookie = http.cookies.SimpleCookie()
        cookie.load(login_headers.get("Set-Cookie", ""))
        auth_cookie = next(iter(cookie.values()), None)
        if auth_cookie is None or not auth_cookie.key.startswith("__Host-rdr-"):
            raise ScenarioError("login cookie was not host scoped")
        auth_pair = f"{auth_cookie.key}={auth_cookie.value}"
        code, _, body = request(first_url, ca_cert, mappings, "/identity", headers={"Cookie": auth_pair})
        if code != 200 or json.loads(body).get("role") != "reports":
            raise ScenarioError("authenticated API request did not reach reports")
        code, app_headers, body = request(first_url, ca_cert, mappings, "/session", headers={"Cookie": auth_pair})
        if code != 200 or "mini_session=secure-fixture" not in app_headers.get("Set-Cookie", ""):
            raise ScenarioError("application cookie was not preserved")
        cookie_header = auth_pair + "; mini_session=secure-fixture"
        code, _, body = request(first_url, ca_cert, mappings, "/session", headers={"Cookie": cookie_header})
        if code != 200 or json.loads(body).get("roundtrip") is not True:
            raise ScenarioError("application cookie roundtrip failed through the share")
        code, _, body = request(first_url, ca_cert, mappings, "/catalog/identity", headers={"Cookie": auth_pair})
        if code != 200 or Path(json.loads(body)["worktree"]).resolve() != repo.resolve():
            raise ScenarioError("authenticated explicit dependency route did not reach the pinned catalog")
        websocket(first_url, ca_cert, mappings, auth_pair)

        repeated = cli(repo, "share", "reports", "--project", project, "--worktree", str(repo), "--mode", "live")
        if repeated.get("credentials") or repeated["results"][0].get("url") != first_url:
            raise ScenarioError("repeat share rotated the URL or returned the password again")
        check_publication_output(repo, project, password)

        # A failed replacement must keep the original tunnel and app processes intact.
        ids_before_failure = run_ids(repo, project)
        Path(ENV["FAKE_CF_FAIL_NEXT"]).touch()
        failed = cli(repo, "share", "reports", "--project", project, "--worktree", str(repo), "--mode", "live", "--recreate", ok=False)
        if failed.get("status") not in ("error", "partial"):
            raise ScenarioError("forced tunnel startup failure returned an unexpected state")
        if len(publications(repo, project)) != 1 or publications(repo, project)[0].get("url") != first_url:
            raise ScenarioError("failed replacement displaced the existing publication")
        if run_ids(repo, project) != ids_before_failure:
            raise ScenarioError("failed replacement changed application run IDs")
        if request(first_url, ca_cert, mappings, "/identity", headers={"Cookie": auth_pair})[0] != 200:
            raise ScenarioError("failed replacement disrupted the existing authenticated publication")

        recreated = cli(repo, "share", "reports", "--project", project, "--worktree", str(repo), "--mode", "live", "--recreate")
        new_password = recreated.get("credentials", {}).get("password", "")
        new_url = recreated.get("results", [{}])[0].get("url", "")
        if not new_password or new_password == password or new_url == first_url:
            raise ScenarioError("recreate did not rotate both URL and password")
        if json.dumps(recreated).count(new_password) != 1:
            raise ScenarioError("recreated password was not exposed exactly once")
        if request(new_url, ca_cert, mappings, "/identity", headers={"Cookie": auth_pair})[0] != 303:
            raise ScenarioError("old authenticated cookie was accepted by the recreated share")
        check_publication_output(repo, project, new_password)

        before_unshare = run_ids(repo, project)
        cli(repo, "unshare", "reports", "--project", project, "--worktree", str(repo))
        if publications(repo, project) or run_ids(repo, project) != before_unshare:
            raise ScenarioError("unshare removed app processes or left a publication listed")

        # Configured snapshot is the default, while the earlier live share proves explicit override.
        snapshot = cli(repo, "share", "reports", "--project", project, "--worktree", str(repo))
        snapshot_password = snapshot.get("credentials", {}).get("password", "")
        snapshot_url = snapshot.get("results", [{}])[0].get("url", "")
        if not snapshot_password or not snapshot_url.startswith("https://") or json.dumps(snapshot).count(snapshot_password) != 1:
            raise ScenarioError("configured snapshot did not return its one-time password and HTTPS URL")
        host, _ = endpoint(snapshot_url, mappings)
        origin = "https://" + host
        code, headers, _ = request(snapshot_url, ca_cert, mappings, "/")
        if code != 303 or not headers.get("Location", "").startswith("/_rdr/login"):
            raise ScenarioError("anonymous snapshot static request bypassed password authentication")
        form = urlencode({"password": snapshot_password, "next": "/"})
        code, login_headers, _ = request(snapshot_url, ca_cert, mappings, "/_rdr/login", "POST",
                                         {"Origin": origin, "Content-Type": "application/x-www-form-urlencoded"}, form)
        if code != 303 or "Secure" not in login_headers.get("Set-Cookie", ""):
            raise ScenarioError("snapshot login did not set a Secure session cookie")
        snapshot_cookie = http.cookies.SimpleCookie()
        snapshot_cookie.load(login_headers.get("Set-Cookie", ""))
        snapshot_auth = next(iter(snapshot_cookie.values()), None)
        if snapshot_auth is None:
            raise ScenarioError("snapshot login omitted its session cookie")
        snapshot_auth_pair = f"{snapshot_auth.key}={snapshot_auth.value}"

        def snapshot_page(url, cookie):
            status, _, body = request(url, ca_cert, mappings, "/", headers={"Cookie": cookie})
            if status != 200:
                raise ScenarioError(f"authenticated snapshot returned HTTP {status}")
            return json.loads(body), body

        page_a, body_a = snapshot_page(snapshot_url, snapshot_auth_pair)
        if page_a.get("marker") != "snapshot-source-A" or page_a.get("public_url") != snapshot_url or page_a.get("catalog_url") != snapshot_url + "/catalog" or page_a.get("share_override") != "synthetic-share-override":
            raise ScenarioError(f"snapshot did not receive expected publication environment: {page_a}")
        if "127.0.0.1" in body_a.decode() or "direct_url" in body_a.decode():
            raise ScenarioError("snapshot artifact exposed a local dependency address")
        code, _, _ = request(snapshot_url, ca_cert, mappings, "/identity")
        if code != 303:
            raise ScenarioError("anonymous snapshot API request bypassed authentication")
        code, _, _ = request(snapshot_url, ca_cert, mappings, "/catalog/socket", headers={"Connection": "Upgrade", "Upgrade": "websocket"})
        if code != 303:
            raise ScenarioError("anonymous snapshot WebSocket bypassed authentication")
        code, _, _ = request(snapshot_url, ca_cert, mappings, "/catalog/identity", headers={"Cookie": snapshot_auth_pair})
        if code != 200:
            raise ScenarioError("snapshot dependency route did not reach its pinned catalog")
        websocket(snapshot_url, ca_cert, mappings, snapshot_auth_pair, path="/catalog/socket")

        # Published bytes stay immutable after the source tree changes or disappears.
        (repo / "source-marker.txt").write_text("snapshot-source-B\n")
        shutil.rmtree(repo / "dist")
        if snapshot_page(snapshot_url, snapshot_auth_pair)[1] != body_a:
            raise ScenarioError("editing/deleting the source artifact changed the active snapshot")

        (repo / "snapshot-fail-next").touch()
        build_failure = cli(repo, "share", "reports", "--project", project, "--worktree", str(repo), "--recreate", ok=False)
        if build_failure.get("status") != "error" or not build_failure.get("errors") or build_failure["errors"][0].get("code") != "BUILD_FAILED" or build_failure.get("credentials"):
            raise ScenarioError(f"failed snapshot build returned unsafe/unexpected result: {build_failure}")
        failed_result = build_failure.get("results", [{}])[0]
        if failed_result.get("state") != "failed" or not any("build.log" in path for path in failed_result.get("log_paths", [])):
            raise ScenarioError("failed snapshot did not preserve its safe build log path")
        if len(publications(repo, project)) != 1 or publications(repo, project)[0].get("url") != snapshot_url:
            raise ScenarioError("failed snapshot recreate displaced the active publication")
        if run_ids(repo, project) != before_unshare:
            raise ScenarioError("failed snapshot recreate changed application run IDs")
        if snapshot_page(snapshot_url, snapshot_auth_pair)[1] != body_a:
            raise ScenarioError("failed snapshot recreate changed the old session or artifact")

        snapshot_b = cli(repo, "share", "reports", "--project", project, "--worktree", str(repo), "--recreate")
        password_b = snapshot_b.get("credentials", {}).get("password", "")
        url_b = snapshot_b.get("results", [{}])[0].get("url", "")
        if not password_b or password_b == snapshot_password or url_b == snapshot_url or json.dumps(snapshot_b).count(password_b) != 1:
            raise ScenarioError("successful snapshot recreate did not rotate URL and one-time password")
        if request(url_b, ca_cert, mappings, "/", headers={"Cookie": snapshot_auth_pair})[0] != 303:
            raise ScenarioError("recreated snapshot accepted the old session cookie")
        host_b, _ = endpoint(url_b, mappings)
        form = urlencode({"password": password_b, "next": "/"})
        code, headers_b, _ = request(url_b, ca_cert, mappings, "/_rdr/login", "POST",
                                     {"Origin": "https://" + host_b, "Content-Type": "application/x-www-form-urlencoded"}, form)
        if code != 303:
            raise ScenarioError("new snapshot password was rejected")
        cookie_b = http.cookies.SimpleCookie()
        cookie_b.load(headers_b.get("Set-Cookie", ""))
        cookie_value_b = next(iter(cookie_b.values()), None)
        if cookie_value_b is None:
            raise ScenarioError("new snapshot login omitted its session cookie")
        auth_b = f"{cookie_value_b.key}={cookie_value_b.value}"
        page_b, body_b = snapshot_page(url_b, auth_b)
        if page_b.get("marker") != "snapshot-source-B" or page_b.get("public_url") != url_b or page_b.get("catalog_url") != url_b + "/catalog" or page_b.get("share_override") != "synthetic-share-override":
            raise ScenarioError(f"recreated snapshot did not use updated source/environment: {page_b}")
        if "127.0.0.1" in body_b.decode() or "direct_url" in body_b.decode():
            raise ScenarioError("recreated snapshot artifact exposed a local dependency address")
        shutil.rmtree(repo / "dist")
        if snapshot_page(url_b, auth_b)[1] != body_b:
            raise ScenarioError("deleting the rebuilt source artifact changed the recreated snapshot")
        check_publication_output(repo, project, password_b)
        before_unshare = run_ids(repo, project)
        cli(repo, "unshare", "reports", "--project", project, "--worktree", str(repo))
        if publications(repo, project) or run_ids(repo, project) != before_unshare:
            raise ScenarioError("snapshot unshare changed app runs or left a publication listed")

        cli(repo, "share", "reports", "--project", project, "--worktree", str(repo), "--mode", "live")
        cli(repo, "restart", "catalog", "--project", project, "--worktree", str(repo))
        if publications(repo, project):
            raise ScenarioError("restarting a routed dependency left its publication active")
        passed = True
    except Exception as exc:
        print(f"INCOMPLETE: {exc}", file=sys.stderr)
    finally:
        if registered:
            try:
                cli(repo, "down", "--all")
                status = cli(repo, "status", "--project", project)
                bad = [item for item in status.get("results", []) if item.get("state_unknown") or
                       item.get("state") != "stopped" or item.get("preparation")]
                if status.get("status") != "ok" or bad or status.get("publications"):
                    raise ScenarioError(f"cleanup not confirmed: apps={bad} publications={status.get('publications')}")
            except Exception as exc:
                print(f"CLEANUP FAILED: {exc}", file=sys.stderr)
                print(f"Fixture and runtime retained at {temp}", file=sys.stderr)
                raise SystemExit(3) from exc
        shutil.rmtree(temp)
    if passed:
        print("PASS: offline public live/snapshot shares, auth, artifact rotation, unshare, routed restart and cleanup")
        return 0
    return 2


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as exc:
        print(f"INCOMPLETE: setup failed: {exc}", file=sys.stderr)
        raise SystemExit(2)
