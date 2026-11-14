#!/usr/bin/env python3
"""Build a tiny synthetic static artifact for the offline publication scenario."""
import json
import os
import sys
from pathlib import Path


def main():
    fail_marker = Path("snapshot-fail-next")
    if fail_marker.exists():
        fail_marker.unlink()
        print("synthetic snapshot build failure", file=sys.stderr)
        return 29
    artifact = Path("dist")
    artifact.mkdir(exist_ok=True)
    page = {
        "marker": Path("source-marker.txt").read_text().strip(),
        "public_url": os.environ.get("RDR_URL", ""),
        "catalog_url": os.environ.get("RDR_DEP_CATALOG_URL", ""),
        "share_override": os.environ.get("SHARE_ONLY", ""),
    }
    (artifact / "index.html").write_text(json.dumps(page, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
