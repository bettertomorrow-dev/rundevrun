#!/usr/bin/env python3
"""Exercise switch forms using only rdr's public JSON interface."""
import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
ENV = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
ENV.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
RDR = os.environ.get("RDR_BIN") or (sys.argv[1] if len(sys.argv) > 1 else "rdr")
RDR = shutil.which(RDR) or str(Path(RDR).expanduser().resolve())


class Incomplete(RuntimeError):
    pass


def run_cli(cwd, *args, ok=True):
    p = subprocess.run([RDR, *args, "--json"], cwd=cwd, env=ENV, text=True,
                       capture_output=True, timeout=40)
    try:
        data = json.loads(p.stdout)
    except json.JSONDecodeError as exc:
        raise Incomplete(f"rdr {args[0]} returned invalid JSON: {p.stderr.strip()}") from exc
    if ok and p.returncode:
        errors = "; ".join(x.get("message", "") for x in data.get("errors", []))
        raise Incomplete(f"rdr {' '.join(args)} failed ({p.returncode}): {errors}")
    if not ok and p.returncode == 0:
        raise Incomplete(f"rdr {' '.join(args)} unexpectedly succeeded")
    return data


def git(cwd, *args):
    subprocess.run(["git", *args], cwd=cwd, env=ENV, check=True,
                   stdout=subprocess.DEVNULL, timeout=30)


def scope(project, root):
    return ["--project", project, "--worktree", str(root)]


def public_instances(cwd, project):
    data = run_cli(cwd, "status", "--project", project, "--worktree", str(cwd))
    if data["status"] != "ok":
        raise Incomplete(f"status failed: {data.get('errors')}")
    return {x["service"]: x for x in data["results"]}


def run_ids(cwd, project):
    return {x["instance_id"]: x.get("run_id") for x in
            run_cli(cwd, "status", "--project", project)["results"]}


def main():
    temp = Path(tempfile.mkdtemp(prefix="rdr-mini-switch-"))
    repo, runtime = temp / "repo", temp / "runtime"
    ENV["RDR_HOME"] = str(runtime)
    roots = {"main": repo, "a": temp / "wt-a", "b": temp / "wt-b", "c": temp / "wt-c"}
    registered = False
    passed = False
    try:
        repo.mkdir()
        for name in ("dev.toml", "service.py"):
            shutil.copyfile(HERE / name, repo / name)
        template = temp / "empty-template"
        template.mkdir()
        git(repo, "init", "--initial-branch=main", f"--template={template}")
        for key, value in (("core.hooksPath", "/dev/null"), ("core.fsmonitor", "false"),
                           ("commit.gpgSign", "false"), ("user.name", "rdr example"),
                           ("user.email", "example@invalid")):
            git(repo, "config", key, value)
        git(repo, "add", "dev.toml", "service.py")
        git(repo, "commit", "-m", "synthetic mini services")
        for name in ("a", "b", "c"):
            git(repo, "worktree", "add", "-b", f"switch-{name}", str(roots[name]))

        registered_result = run_cli(repo, "register", ".")
        registered = True
        project = registered_result["scope"]["project"]
        listing = run_cli(repo, "worktrees", "--project", project)
        discovered = {Path(x["root"]).resolve(): Path(x["root"]).resolve()
                      for x in listing["results"] if x.get("root")}
        if set(roots.values()) != set(discovered):
            raise Incomplete("worktrees JSON did not list the four fixture roots")
        roots = {key: discovered[path.resolve()] for key, path in roots.items()}
        for path in roots.values():
            run_cli(path, "config", "check", "--project", project, "--worktree", str(path))

        run_cli(repo, "up", "catalog", "reports", "indexer", *scope(project, repo))
        run_cli(roots["c"], "up", "catalog", *scope(project, roots["c"]))
        run_cli(roots["c"], "bind", "reports", "catalog", *scope(project, roots["c"]),
                "--to-worktree", str(roots["c"]))
        run_cli(roots["c"], "up", "reports", *scope(project, roots["c"]))
        run_cli(roots["c"], "bind", "indexer", "catalog", *scope(project, roots["c"]),
                "--to-worktree", str(repo))
        independent_before = public_instances(roots["c"], project)
        independent_ids = {name: independent_before[name].get("run_id") for name in ("catalog", "reports")}
        stopped_indexer_id = independent_before["indexer"]["instance_id"]
        initial = run_ids(repo, project)
        # Unknown final selector must be rejected before any root is touched.
        run_cli(repo, "switch", "reports", "indexer", "missing", *scope(project, repo),
                "--to-worktree", str(roots["a"]), ok=False)
        if run_ids(repo, project) != initial:
            raise Incomplete("invalid final service selector changed live run IDs")

        bad_file = temp / "invalid-switch.json"
        bad_file.write_text(json.dumps({"version": 1, "mappings": [
            {"from": {"project": project, "worktree": str(repo), "service": "reports"},
             "to": {"project": project, "worktree": str(roots["a"]), "service": "reports"}},
            {"from": {"project": project, "worktree": str(repo), "service": "missing"},
             "to": {"project": project, "worktree": str(roots["b"]), "service": "missing"}},
        ]}))
        run_cli(repo, "switch", "--file", str(bad_file), ok=False)
        if run_ids(repo, project) != initial:
            raise Incomplete("invalid final file mapping changed live run IDs")

        switched = run_cli(repo, "switch", "catalog", *scope(project, repo), "--to-worktree", str(roots["a"]))
        after_independent = public_instances(roots["c"], project)
        stopped_indexer = after_independent["indexer"]
        target_catalog_id = public_instances(roots["a"], project)["catalog"]["instance_id"]
        if stopped_indexer.get("run_id") or stopped_indexer.get("state") != "stopped" or \
                stopped_indexer.get("desired_bindings", {}).get("catalog") != target_catalog_id:
            raise Incomplete(f"stopped indexer binding did not follow switched catalog: {stopped_indexer}")
        rewritten = [x for x in switched.get("results", []) if x.get("instance_id") == stopped_indexer_id]
        if len(rewritten) != 1 or rewritten[0].get("status") != "completed":
            raise Incomplete("switch did not report the stopped indexer metadata rewrite")
        if any(after_independent[name].get("run_id") != independent_ids[name] for name in independent_ids):
            raise Incomplete("switch changed the independent C catalog or reports run")

        run_cli(roots["main"], "switch", "reports", "indexer", *scope(project, roots["main"]), "--to-worktree", str(roots["b"]))

        # A file can map services from one worktree to different targets.
        run_cli(roots["c"], "down", "reports", *scope(project, roots["c"]))
        mapping_file = temp / "mixed-switch.json"
        mapping_file.write_text(json.dumps({"version": 1, "mappings": [
            {"from": {"project": project, "worktree": str(roots["b"]), "service": "reports"},
             "to": {"project": project, "worktree": str(roots["c"]), "service": "reports"}},
            {"from": {"project": project, "worktree": str(roots["b"]), "service": "indexer"},
             "to": {"project": project, "worktree": str(roots["a"]), "service": "indexer"}},
        ]}))
        run_cli(repo, "switch", "--file", str(mapping_file))
        run_cli(roots["c"], "down", "reports", *scope(project, roots["c"]))
        run_cli(roots["c"], "down", "catalog", *scope(project, roots["c"]))
        # The remaining live source services are moved by the explicit all-services selector.
        run_cli(roots["b"], "up", "catalog", *scope(project, roots["b"]))
        run_cli(roots["b"], "switch", "--all-services", *scope(project, roots["b"]),
                "--to-worktree", str(roots["c"]))

        # A failed target build must leave the old catalog run intact.
        before = public_instances(roots["c"], project)
        old_id = before["catalog"].get("run_id")
        if not old_id:
            raise Incomplete("public status omitted the source catalog run ID")
        target_config = roots["a"] / "dev.toml"
        config_text = target_config.read_text()
        target_config.write_text(config_text.replace('command = "python3 service.py catalog"',
                                                      'command = "python3 service.py catalog"\nbuild = "false"', 1))
        failed = run_cli(roots["c"], "switch", "catalog", *scope(project, roots["c"]),
                         "--to-worktree", str(roots["a"]), ok=False)
        if failed.get("status") not in ("error", "partial"):
            raise Incomplete(f"failed target build returned unexpected status {failed.get('status')}")
        after = public_instances(roots["c"], project)
        if after["catalog"].get("run_id") != old_id or after["catalog"].get("state") not in ("running", "ready"):
            raise Incomplete("failed target build did not preserve the source catalog run")
        passed = True
    except Exception as exc:
        print(f"INCOMPLETE: {exc}", file=sys.stderr)
    finally:
        if registered:
            try:
                # The isolated RDR_HOME makes this global stop safe for this fixture.
                run_cli(repo, "down", "--all", ok=True)
                data = run_cli(repo, "status", "--project", project)
                bad = [x for x in data.get("results", []) if x.get("state_unknown") or
                       x.get("state") != "stopped" or x.get("preparation")]
                if data.get("status") != "ok" or bad:
                    raise Incomplete(f"status did not confirm stopped instances: {bad}")
            except Exception as exc:
                print(f"CLEANUP FAILED: {exc}", file=sys.stderr)
                print(f"Runtime and fixture retained at {temp}", file=sys.stderr)
                raise SystemExit(3) from exc
        shutil.rmtree(temp)
    if passed:
        print("PASS: public switch forms, preflight rejection, and failed-build preservation; cleanup confirmed")
        return 0
    return 2


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, subprocess.CalledProcessError) as exc:
        print(f"INCOMPLETE: setup failed: {exc}", file=sys.stderr)
        raise SystemExit(2)
