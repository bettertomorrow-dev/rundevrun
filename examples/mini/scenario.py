#!/usr/bin/env python3
"""Exercise the public rdr JSON CLI against disposable Git worktrees."""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
# Never let ambient Git selectors/config redirect commands or run configured hooks.
FIXTURE_ENV = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
FIXTURE_ENV.update({"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull})
CLI_ENV = FIXTURE_ENV.copy()
RDR = os.environ.get("RDR_BIN") or (sys.argv[1] if len(sys.argv) > 1 else "rdr")
if os.sep in RDR:
    RDR = str(Path(RDR).expanduser().resolve())
else:
    RDR = shutil.which(RDR) or RDR


class ScenarioError(RuntimeError):
    pass


def main():
    temp = Path(tempfile.mkdtemp(prefix="rdr-mini-"))
    repo = temp / "repo"
    state = temp / "state"
    roots = {"main": repo, "consumer-a": temp / "consumer-a", "consumer-b": temp / "consumer-b", "catalog-c": temp / "catalog-c"}
    registered = False
    config_checked = False
    scenario_passed = False
    project = None
    try:
        repo.mkdir()
        CLI_ENV["RDR_HOME"] = str(state)
        FIXTURE_ENV["RDR_HOME"] = str(state)
        for name in ("dev.toml", "service.py"):
            shutil.copyfile(HERE / name, repo / name)

        template = temp / "empty-git-template"
        template.mkdir()
        git(repo, "init", "--initial-branch=main", f"--template={template}")
        git(repo, "config", "core.hooksPath", "/dev/null")
        git(repo, "config", "core.fsmonitor", "false")
        git(repo, "config", "commit.gpgSign", "false")
        git(repo, "config", "user.name", "rdr example")
        git(repo, "config", "user.email", "example@invalid")
        git(repo, "add", "dev.toml", "service.py")
        git(repo, "commit", "-m", "mini example")
        git(repo, "worktree", "add", "-b", "consumer-a", str(roots["consumer-a"]))
        git(repo, "worktree", "add", "-b", "consumer-b", str(roots["consumer-b"]))
        git(repo, "worktree", "add", "-b", "catalog-c", str(roots["catalog-c"]))

        result = cli(repo, "register", ".")
        registered = True
        project = result["scope"]["project"]
        found = cli(repo, "worktrees", "--project", project)
        public_worktrees = {item.get("branch"): Path(item["root"]).resolve() for item in found["results"] if item.get("root")}
        expected_branches = set(roots)
        if set(public_worktrees) != expected_branches:
            raise ScenarioError("public worktrees JSON did not list the four expected fixture branches")
        roots = public_worktrees
        for path in roots.values():
            cli(path, "config", "check", "--project", project, "--worktree", str(path))
        config_checked = True

        # Share the main catalog between reports in A and indexer in A, plus reports in B.
        cli(roots["consumer-a"], "bind", "reports", "catalog", *scope(project, roots["consumer-a"]), "--to-worktree", str(roots["main"]))
        cli(roots["consumer-a"], "bind", "indexer", "catalog", *scope(project, roots["consumer-a"]), "--to-worktree", str(roots["main"]))
        cli(roots["consumer-b"], "bind", "reports", "catalog", *scope(project, roots["consumer-b"]), "--to-worktree", str(roots["main"]))
        cli(roots["main"], "up", "catalog", *scope(project, roots["main"]))
        cli(roots["consumer-a"], "up", "reports", "indexer", *scope(project, roots["consumer-a"]))
        cli(roots["consumer-b"], "up", "reports", *scope(project, roots["consumer-b"]))

        # The third checkout has its own catalog. Rebind only A's reports and restart it.
        cli(roots["catalog-c"], "up", "catalog", *scope(project, roots["catalog-c"]))
        cli(roots["consumer-a"], "bind", "reports", "catalog", *scope(project, roots["consumer-a"]), "--to-worktree", str(roots["catalog-c"]))
        cli(roots["consumer-a"], "restart", "reports", *scope(project, roots["consumer-a"]))
        status = cli(roots["consumer-a"], "status", "--project", project, "--worktree", str(roots["consumer-a"]))
        reports = one_instance(status, "reports")
        reports_url = public_url(reports)
        observed = get_json(reports_url + "/identity")["catalog"]["worktree"]
        if Path(observed).resolve() != roots["catalog-c"].resolve():
            raise ScenarioError(f"reports reached {observed}, expected selected catalog {roots['catalog-c']}")

        cli(roots["consumer-b"], "down", "reports", *scope(project, roots["consumer-b"]))
        assert_http(public_url(one_instance(cli(roots["main"], "status", "--project", project, "--worktree", str(roots["main"])), "catalog")) + "/health")
        assert_http(public_url(one_instance(cli(roots["consumer-a"], "status", "--project", project, "--worktree", str(roots["consumer-a"])), "reports")) + "/health")

        guarded = cli(roots["main"], "down", "catalog", *scope(project, roots["main"]), check=False)
        if guarded["status"] == "ok":
            raise ScenarioError("down catalog succeeded while indexer still consumes it")
        cli(roots["main"], "down", "catalog", "--cascade", *scope(project, roots["main"]))
        scenario_passed = True
    except Exception as exc:
        print(f"INCOMPLETE: {exc}", file=sys.stderr)
        if config_checked:
            print("The fixture and config checks ran; graph coverage needs the stage 3 CLI.", file=sys.stderr)
        else:
            print("Fixture setup stopped before config checks completed.", file=sys.stderr)
    finally:
        if registered and project:
            try:
                cleanup(repo, project, roots)
            except Exception as exc:
                print(f"CLEANUP FAILED: {exc}", file=sys.stderr)
                print(f"Runtime and fixture retained at {temp}", file=sys.stderr)
                raise SystemExit(3) from exc
        shutil.rmtree(temp, ignore_errors=True)
    if scenario_passed:
        print("PASS: public JSON CLI completed multi-worktree dependency scenario")
        return 0
    return 2


def scope(project, root):
    return ["--project", project, "--worktree", str(root)]


def git(cwd, *args):
    subprocess.run(["git", *args], cwd=cwd, env=FIXTURE_ENV, check=True, stdout=subprocess.DEVNULL, timeout=30)


def cli(cwd, *args, check=True):
    proc = subprocess.run([RDR, *args, "--json"], cwd=cwd, env=CLI_ENV, text=True, capture_output=True, timeout=30)
    try:
        result = json.loads(proc.stdout)
    except json.JSONDecodeError as exc:
        raise ScenarioError(f"rdr {args[0]} did not return public JSON: {proc.stderr.strip()}") from exc
    if check and proc.returncode:
        errors = "; ".join(error.get("message", str(error)) for error in result.get("errors", []))
        raise ScenarioError(f"rdr {' '.join(args)} failed ({proc.returncode}): {errors}")
    return result


def one_instance(result, service):
    matches = [item for item in result.get("results", []) if item.get("service") == service and item.get("worktree_id")]
    if len(matches) != 1:
        raise ScenarioError(f"public JSON did not identify one {service} instance in the selected worktree")
    return matches[0]


def public_url(instance):
    # Service checks use direct_url, never the routed browser URL.
    urls = instance.get("urls", {})
    address = instance.get("address", {})
    url = instance.get("direct_url")
    if not url and isinstance(urls, dict):
        url = urls.get("direct_url")
    if not url and isinstance(address, dict):
        url = address.get("direct_url")
    if not url and isinstance(address, dict) and address.get("host") and address.get("port"):
        url = f"http://{address['host']}:{address['port']}"
    if not url:
        raise ScenarioError(f"public JSON omitted direct_url for {instance.get('instance_id')}")
    return url.rstrip("/")


def get_json(url):
    with urllib.request.urlopen(url, timeout=5) as response:
        return json.load(response)


def assert_http(url):
    with urllib.request.urlopen(url, timeout=5) as response:
        if response.status != 200:
            raise ScenarioError(f"health check failed: {url}")


def cleanup(repo, project, roots):
    # Stop consumers first. Each action goes through the public CLI, never a saved PID.
    for root, services in (
        (roots["consumer-b"], ("reports",)),
        (roots["consumer-a"], ("reports", "indexer")),
        (roots["catalog-c"], ("catalog",)),
        (roots["main"], ("catalog",)),
    ):
        for service in services:
            cli(root, "down", service, *scope(project, root), check=False)

    result = cli(repo, "status", "--project", project)
    active = []
    for item in result.get("results", []):
        state = item.get("state")
        if item.get("state_unknown") or state != "stopped" or item.get("preparation"):
            active.append(f"{item.get('instance_id', '<unknown>')} state={state!r} preparation={item.get('preparation')!r}")
    if result.get("status") != "ok" or active:
        detail = "; ".join(active) or "; ".join(error.get("message", str(error)) for error in result.get("errors", []))
        raise ScenarioError("public status did not confirm a clean runtime" + (f": {detail}" if detail else ""))


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, subprocess.CalledProcessError) as exc:
        print(f"INCOMPLETE: fixture setup failed: {exc}", file=sys.stderr)
        raise SystemExit(2)
