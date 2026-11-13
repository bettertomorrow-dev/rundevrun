#!/usr/bin/env python3
"""Run MillDrum and twikko together using public rdr JSON and disposable data."""
import http.client
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from urllib.parse import urlencode, urlsplit


class Incomplete(RuntimeError):
    pass


def main():
    if len(sys.argv) != 4:
        print("Usage: real_projects_scenario.py RDR MILLDRUM_REPO TWIKKO_REPO", file=sys.stderr)
        return 2
    binary, milldrum, twikko = [Path(value).expanduser().resolve() for value in sys.argv[1:]]
    temp = Path(tempfile.mkdtemp(prefix="rdr-real-projects-"))
    for name in ("runtime", "config", "state"):
        (temp / name).mkdir(mode=0o700)
    env = {key: value for key, value in os.environ.items()
           if not key.startswith(("MILLDRUM_", "RDR_", "TWIKKO_", "GIT_"))}
    env.update(RDR_HOME=str(temp / "runtime"), XDG_CONFIG_HOME=str(temp / "config"),
               XDG_STATE_HOME=str(temp / "state"), MILLDRUM_DEV_DATA_HOME=str(temp / "milldrum-data"),
               TWIKKO_INPUT=str(temp / "input records.yaml"), TWIKKO_OUTPUT=str(temp / "saved result.yaml"),
               GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
    Path(env["TWIKKO_INPUT"]).write_text("""title: Local runner acceptance
schema:
  id_field: id
  fields:
    - name: id
      type: text
      readonly: true
    - name: summary
      type: text
      required: true
entities:
  - id: DEMO-1001
    summary: Synthetic record
""")
    registered = False
    passed = False
    worktree_for = {}

    def cli(cwd, *args, expect=0):
        proc = subprocess.run([str(binary), *map(str, args), "--json"], cwd=cwd, env=env,
                              capture_output=True, text=True, timeout=240)
        try:
            result = json.loads(proc.stdout)
        except ValueError as exc:
            raise Incomplete(f"{args[0]} returned invalid JSON") from exc
        expected = expect if isinstance(expect, tuple) else (expect,)
        if result.get("version") != 1 or proc.returncode not in expected:
            codes = [item.get("code") for item in result.get("errors", [])]
            raise Incomplete(f"{args[0]} exit {proc.returncode}, codes={codes}")
        return result

    def one(results, service):
        items = [item for item in results if item.get("service") == service]
        if len(items) != 1:
            raise Incomplete(f"expected one {service} result")
        return items[0]

    def status(cwd, project):
        return cli(cwd, "status", "--project", project, "--worktree", worktree_for[project])["results"]

    def request(url, suffix="", form=None):
        parsed = urlsplit(url)
        connection = http.client.HTTPConnection(parsed.hostname, parsed.port, timeout=5)
        connection.request("GET" if form is None else "POST", parsed.path + suffix,
                           body=None if form is None else urlencode(form),
                           headers={"Content-Type": "application/x-www-form-urlencoded"})
        response = connection.getresponse()
        body = response.read(1 << 20)
        code = response.status
        connection.close()
        if code != 200:
            raise Incomplete(f"application HTTP response {code}")
        return body

    def editor_url(project):
        logs = cli(twikko, "logs", "editor", "--project", project, "--tail", "20")
        text = logs["results"][0]["logs"]
        matches = re.findall(r"multieditor: url=(http://127\.0\.0\.1:\d+/s/[a-zA-Z0-9_-]+/)", text)
        if len(matches) != 1:
            raise Incomplete("editor did not advertise one session URL in public logs")
        return matches[0]

    def terminal(project):
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            item = one(status(twikko, project), "editor")
            if item.get("state_unknown"):
                raise Incomplete("editor process ownership became unknown")
            if item.get("state") in ("failed", "stopped"):
                return item["state"]
            time.sleep(0.1)
        raise Incomplete("editor did not exit after its session completed")

    def assert_port_closed(url):
        parsed = urlsplit(url)
        try:
            connection = socket.create_connection((parsed.hostname, parsed.port), timeout=0.5)
        except ConnectionRefusedError:
            return
        connection.close()
        raise Incomplete("editor listener remained after completion")

    try:
        version = cli(milldrum, "version")
        if version["results"][0].get("api_version") != 1:
            raise Incomplete("rdr public API 1 is required")
        cli(milldrum, "--help")
        selected = []
        for cwd in (milldrum, twikko):
            cli(cwd, "config", "check")
            cli(cwd, "register", cwd, "--dry-run", expect=(0, 5))
            registration = cli(cwd, "register", cwd, expect=(0, 5))
            registered = True
            project = registration["scope"]["project"]
            worktree = next(item["id"] for item in registration["results"] if item.get("root") == str(cwd) and item.get("project_id") == project)
            if not any(item.get("id") == worktree and item.get("config_valid") for item in registration["results"]):
                raise Incomplete("the selected worktree configuration is invalid")
            worktree_for[project] = worktree
            cli(cwd, "services", "--project", project, "--worktree", worktree)
            selected.append((project, worktree))
        (mp, mw), (tp, tw) = selected
        cli(milldrum, "up", "backend", "ui", "--project", mp, "--worktree", mw, "--dry-run")
        cli(twikko, "up", "editor", "--project", tp, "--worktree", tw, "--dry-run")
        cli(milldrum, "up", "backend", "ui", "--project", mp, "--worktree", mw, "--no-proxy")
        cli(twikko, "up", "editor", "--project", tp, "--worktree", tw, "--no-proxy")
        apps = cli(milldrum, "ls")["results"]
        active = [item for item in apps if item.get("state") in ("starting", "running", "ready", "unhealthy")]
        if len(active) != 3 or any(item.get("state") != "ready" or item.get("state_unknown") for item in active):
            raise Incomplete("the two real projects are not simultaneously ready")
        before = {item["instance_id"]: item["run_id"] for item in active}
        api = one(status(milldrum, mp), "backend")
        health = json.loads(request(api["direct_url"] + "/v1/health"))
        if health["data"]["status"] != "ok":
            raise Incomplete("MillDrum did not report healthy")
        cli(milldrum, "bind", "ui", "backend", "--project", mp, "--worktree", mw, "--to-worktree", mw)
        cli(milldrum, "share", "ui", "--project", mp, "--worktree", mw, "--dry-run")
        missing = cli(twikko, "up", "missing-service", "--project", tp, "--worktree", tw, expect=2)
        if not missing.get("errors") or not missing["errors"][0].get("code"):
            raise Incomplete("invalid selection omitted its public error code")
        observed = {item["instance_id"]: item.get("run_id") for item in cli(milldrum, "ls")["results"]}
        if any(observed.get(instance) != run_id for instance, run_id in before.items()):
            raise Incomplete("read-only planning or failed selection changed a run")
        print("PASS: real MillDrum and twikko ready under one rdr; public planning, binding and errors", flush=True)

        url = editor_url(tp)
        request(url)
        request(url, "entities/0/fields/summary", {"value": "Accepted synthetic edit"})
        request(url, "save", {})
        saved_state = terminal(tp)
        result = Path(env["TWIKKO_OUTPUT"])
        text = result.read_text()
        if not text.startswith("status: saved\n") or "Accepted synthetic edit" not in text or "field: summary" not in text:
            raise Incomplete("editor save omitted its result or change summary")
        cli(twikko, "down", "editor", "--project", tp, "--worktree", tw)
        assert_port_closed(url)
        if not result.is_file():
            raise Incomplete("down removed the editor result")
        print(f"PASS: twikko edit/save and durable output; observed terminal state={saved_state}", flush=True)

        env["TWIKKO_OUTPUT"] = str(temp / "cancelled result.yaml")
        cli(twikko, "up", "editor", "--project", tp, "--worktree", tw, "--no-proxy")
        url = editor_url(tp)
        request(url, "cancel", {})
        cancelled_state = terminal(tp)
        if Path(env["TWIKKO_OUTPUT"]).read_text() != "status: cancelled\n":
            raise Incomplete("editor cancel output is incorrect")
        cli(twikko, "down", "editor", "--project", tp, "--worktree", tw)
        assert_port_closed(url)
        after = status(milldrum, mp)
        if any(before[item["instance_id"]] != item["run_id"] or item.get("state") != "ready" for item in after):
            raise Incomplete("editor session lifecycle disturbed MillDrum")
        print(f"PASS: twikko cancel; observed terminal state={cancelled_state}; MillDrum runs preserved", flush=True)
        passed = True
    except Exception as exc:
        print(f"INCOMPLETE: {type(exc).__name__}: {exc}; fixture={temp}", file=sys.stderr)
    finally:
        clean = False
        try:
            if registered:
                cli(milldrum, "down", "--all")
                observed = cli(milldrum, "status")
                if any(item.get("state") != "stopped" or item.get("state_unknown") or item.get("preparation") for item in observed["results"]) or observed.get("publications") or observed.get("publication_errors"):
                    raise Incomplete("public cleanup did not confirm all projects stopped")
            clean = True
        except Exception as exc:
            print(f"CLEANUP INCOMPLETE: {exc}; retained {temp}", file=sys.stderr)
        if passed and clean:
            shutil.rmtree(temp)
        else:
            print(f"Disposable diagnostic fixture retained: {temp}", file=sys.stderr)
    if passed and clean:
        print("PASS: two real projects accepted on Linux; all temporary processes stopped")
        return 0
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
