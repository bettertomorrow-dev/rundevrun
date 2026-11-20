#!/usr/bin/env python3
"""Select CI jobs from changed paths, defaulting unfamiliar inputs to full validation."""
import subprocess
import sys
from pathlib import PurePosixPath


def classify(paths):
    selected = set()
    for path in paths:
        name = PurePosixPath(path)
        if path.startswith("internal/guides/"):
            selected.add("go")
            if name.suffix in {".mjs", ".js"}:
                selected.add("node")
            continue
        if path in {"README.md", "AGENTS.md", "RTK.md", "LICENSE"} or (
            name.parts[0] in {"docs", "skills"} and name.suffix in {".md", ".txt"}
        ) or (path.startswith("skills/") and name.name == "openai.yaml"):
            continue
        if name.suffix == ".go" or path in {"go.mod", "go.sum"} or path.startswith("examples/"):
            selected.add("go")
        elif path == ".goreleaser.yaml":
            selected.update({"go", "installer"})
        elif path.endswith((".mjs", ".js")) and path.startswith(("skills/", "scripts/")):
            selected.add("node")
        elif path in {"install.sh", "scripts/test-install.py", "scripts/rdr-local", "scripts/rdr-local-test"} or path.startswith(".githooks/"):
            selected.add("installer")
        else:
            return {"go", "node", "installer"}
    return selected


def main():
    try:
        if len(sys.argv) != 3 or set(sys.argv[1]) == {"0"}:
            selected = {"go", "node", "installer"}
        else:
            output = subprocess.check_output([
                "git", "diff", "--no-renames", "--name-only", "-z", *sys.argv[1:]
            ])
            selected = classify(output.decode().rstrip("\0").split("\0") if output else [])
    except (subprocess.CalledProcessError, UnicodeError, ValueError, IndexError):
        selected = {"go", "node", "installer"}
    for job in ("go", "node", "installer"):
        print(f"{job}={str(job in selected).lower()}")


if __name__ == "__main__":
    main()
