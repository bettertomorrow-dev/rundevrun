#!/usr/bin/env python3
"""Check CI selection for prose, executable helpers, renames, and unknown inputs."""
import importlib.util
import subprocess
import sys
from pathlib import Path

spec = importlib.util.spec_from_file_location("changes", Path(__file__).with_name("ci-changes.py"))
changes = importlib.util.module_from_spec(spec)
spec.loader.exec_module(changes)
full = {"go", "node", "installer"}
for paths, expected in [
    (["README.md", "docs/INSTALL.md", "skills/rdr-onboard/SKILL.md"], set()),
    (["skills/example/agents/openai.yaml"], set()),
    (["skills/example/scripts/helper.mjs"], {"node"}),
    (["internal/guides/onboard/GUIDE.md"], {"go"}),
    (["internal/guides/manifest.json"], {"go"}),
    (["internal/guides/domain/scripts/create-cloudflare-token.mjs"], {"go", "node"}),
    (["scripts/domain-wizard-token.test.mjs", "README.md"], {"node"}),
    (["install.sh", ".githooks/post-merge"], {"installer"}),
    (["go.mod"], {"go"}),
    (["internal/example/data.md"], full),
    (["docs/fixture.json"], full),
    (["examples/mini/service.py"], {"go"}),
    ([".github/workflows/ci.yml"], full),
    ([".goreleaser.yaml"], {"go", "installer"}),
    (["scripts/ci-changes.py"], full),
    (["old.go", "docs/moved.md"], {"go"}),
    (["unknown"], full),
    ([], set()),
]:
    assert changes.classify(paths) == expected, paths
result = subprocess.run([sys.executable, str(Path(__file__).with_name("ci-changes.py")), "missing-ci-revision", "HEAD"], capture_output=True, text=True)
assert result.returncode == 0
assert result.stdout == "go=true\nnode=true\ninstaller=true\n"
print("PASS: CI change selection and unavailable-diff fallback")
