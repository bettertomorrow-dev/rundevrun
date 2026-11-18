#!/usr/bin/env bash
# Runs native Linux checks on the WSL filesystem with the CI-selected Go toolchain.
set -euo pipefail
source_dir=$1
go_version=${2#go}
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq ca-certificates curl git build-essential python3
curl -fsSL "https://go.dev/dl/go${go_version}.linux-amd64.tar.gz" -o /tmp/go.tar.gz
curl -fsSL 'https://go.dev/dl/?mode=json&include=all' -o /tmp/go-releases.json
python3 - "$go_version" <<'PYTHON'
import hashlib, json, sys
from pathlib import Path
filename = f"go{sys.argv[1]}.linux-amd64.tar.gz"
releases = json.loads(Path("/tmp/go-releases.json").read_text())
expected = next(file["sha256"] for release in releases for file in release["files"] if file["filename"] == filename)
assert hashlib.sha256(Path("/tmp/go.tar.gz").read_bytes()).hexdigest() == expected, "Go download checksum mismatch"
PYTHON
rm -rf /usr/local/go
tar -C /usr/local -xzf /tmp/go.tar.gz
export PATH=/usr/local/go/bin:$PATH
mkdir -p /root/rundevrun-ci
cp -a "$source_dir/." /root/rundevrun-ci/
cd /root/rundevrun-ci
uname -r | grep -i 'microsoft.*WSL2'
go test -race -count=1 -timeout=10m ./...
python3 scripts/test-mini-service.py
go build -o /tmp/rundevrun ./cmd/rundevrun
python3 examples/mini/scenario.py /tmp/rundevrun
python3 scripts/rdrtop_smoke.py
