#!/usr/bin/env python3
"""Exercise release installation, upgrades, and failures using local download fixtures."""
import hashlib
import io
import os
from pathlib import Path
import platform
import subprocess
import tarfile
import tempfile

repo = Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory(prefix="rundevrun-install-test-") as temporary:
    root = Path(temporary).resolve()
    home, fixtures, tools = root / "home", root / "fixtures", root / "tools"
    for directory in (home, fixtures, tools):
        directory.mkdir()
    curl = tools / "curl"
    curl.write_text("""#!/usr/bin/env python3
import os, pathlib, shutil, sys
args = sys.argv[1:]
if '-w' in args:
    print('https://github.com/bettertomorrow-dev/rundevrun/releases/tag/v0.1.0', end='')
else:
    url = next(arg for arg in args if arg.startswith('https://'))
    shutil.copyfile(pathlib.Path(os.environ['FIXTURES']) / url.rsplit('/', 1)[1], args[args.index('-o') + 1])
""")
    curl.chmod(0o755)
    system = "darwin" if platform.system() == "Darwin" else "linux"
    arch = "arm64" if platform.machine() in ("arm64", "aarch64") else "amd64"
    asset = f"rundevrun_0.1.0_{system}_{arch}.tar.gz"
    env = dict(os.environ, HOME=str(home), FIXTURES=str(fixtures), PATH=f"{tools}:{os.environ['PATH']}")
    env.pop("RUNDEVRUN_INSTALL_DIR", None)
    env.pop("RUNDEVRUN_VERSION", None)
    dest = home / ".local/opt/rundevrun"

    def fixture(label, bad=False):
        with tarfile.open(fixtures / asset, "w:gz") as archive:
            for name in ("rundevrun", "rundevtop"):
                body = f"#!/bin/sh\necho {label}\n".encode()
                if bad and name == "rundevtop":
                    body = b"#!/bin/sh\nexit 1\n"
                info = tarfile.TarInfo(name)
                info.size, info.mode = len(body), 0o755
                archive.addfile(info, io.BytesIO(body))
        checksum = hashlib.sha256((fixtures / asset).read_bytes()).hexdigest()
        (fixtures / "checksums.txt").write_text(f"{checksum}  {asset}\n")

    def invoke(success=True, **overrides):
        result = subprocess.run(["bash", str(repo / "install.sh")], env=dict(env, **overrides), capture_output=True, text=True)
        assert (result.returncode == 0) == success, result.stdout + result.stderr

    fixture("first")
    invoke()
    assert (home / ".local/bin/rundevrun").resolve() == dest / "rundevrun"
    assert (home / ".local/bin/rundevtop").resolve() == dest / "rundevtop"
    fixture("second")
    invoke(RUNDEVRUN_VERSION="v0.1.0")
    for binary in ("rundevrun", "rundevtop"):
        assert "second" in (dest / binary).read_text()
        assert "first" in (dest / f"{binary}.previous").read_text()
    (fixtures / "checksums.txt").write_text(f"wrong  {asset}\n")
    invoke(False)
    fixture("bad", bad=True)
    invoke(False)
    assert "second" in (dest / "rundevrun").read_text()
    assert "first" in (dest / "rundevrun.previous").read_text()
    fixture("third")
    (dest / ".install-lock").mkdir()
    invoke(False)
    assert (dest / ".install-lock").is_dir()
    (dest / ".install-lock").rmdir()
    invoke(False, RUNDEVRUN_VERSION="invalid")
    command = home / ".local/bin/rundevrun"
    command.unlink()
    command.write_text("foreign installation")
    invoke(False)
    assert command.read_text() == "foreign installation"
    custom = root / "custom"
    invoke(RUNDEVRUN_INSTALL_DIR=str(custom))
    assert "third" in (custom / "rundevtop").read_text()
print("PASS: install, update, backups, checksum/smoke failures, lock, foreign commands, custom directory")
