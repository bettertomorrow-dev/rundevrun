#!/usr/bin/env bash
# Downloads and verifies both release binaries before replacing a user-owned installation.
set -euo pipefail
repo=bettertomorrow-dev/rundevrun
case "$(uname -s)" in
  Darwin) platform=darwin ;;
  Linux) platform=linux ;;
  *) echo 'Supported platforms: macOS, Linux, and WSL 2.' >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo 'Supported architectures: amd64 and arm64.' >&2; exit 1 ;;
esac
version=${RUNDEVRUN_VERSION:-}
if [[ -z "$version" ]]; then
  url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest")
  version=${url##*/}
fi
[[ "$version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || { echo 'Expected version vX.Y.Z.' >&2; exit 1; }
asset="rundevrun_${version#v}_${platform}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/$version"
tmp=$(mktemp -d)
lock=
cleanup() {
  rm -rf -- "$tmp"
  [[ -z "$lock" ]] || rm -rf -- "$lock"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
curl -fsSL "$base/$asset" -o "$tmp/$asset"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"
expected=$(awk -v name="$asset" '$2 == name {print $1; exit}' "$tmp/checksums.txt")
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$asset" | awk '{print $1}')
else
  actual=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
fi
[[ -n "$expected" && "$expected" == "$actual" ]] || { echo 'SHA-256 verification failed.' >&2; exit 1; }
tar -xzf "$tmp/$asset" -C "$tmp" rundevrun rundevtop
"$tmp/rundevrun" version --json
"$tmp/rundevrun" --help >/dev/null
"$tmp/rundevtop" --version
"$tmp/rundevtop" --help >/dev/null
dest=${RUNDEVRUN_INSTALL_DIR:-$HOME/.local/opt/rundevrun}
if [[ -z "${RUNDEVRUN_INSTALL_DIR:-}" ]]; then
  for binary in rundevrun rundevtop; do
    link="$HOME/.local/bin/$binary"
    if [[ -e "$link" || -L "$link" ]]; then
      [[ -L "$link" && "$(readlink "$link")" == "$dest/$binary" ]] || {
        echo "Existing command at $link belongs to another installation. Remove it or set RUNDEVRUN_INSTALL_DIR." >&2
        exit 1
      }
    fi
  done
fi
mkdir -p "$dest"
if ! mkdir "$dest/.install-lock" 2>/dev/null; then
  echo "Another install is active, or a stale lock exists: $dest/.install-lock" >&2
  exit 1
fi
lock="$dest/.install-lock"
for binary in rundevrun rundevtop; do
  install -m 755 "$tmp/$binary" "$lock/$binary"
  if [[ -f "$dest/$binary" ]]; then
    cp -p "$dest/$binary" "$lock/$binary.previous"
  elif [[ "$binary" == rundevrun && -f "$dest/rdr" ]]; then
    cp -p "$dest/rdr" "$lock/$binary.previous"
  fi
done
for binary in rundevrun rundevtop; do
  if [[ -f "$lock/$binary.previous" ]]; then
    mv -f "$lock/$binary.previous" "$dest/$binary.previous"
  fi
  mv -f "$lock/$binary" "$dest/$binary"
done
if [[ -z "${RUNDEVRUN_INSTALL_DIR:-}" ]]; then
  mkdir -p "$HOME/.local/bin"
  for binary in rundevrun rundevtop; do
    ln -sfn "$dest/$binary" "$HOME/.local/bin/$binary"
  done
  command_dir="$HOME/.local/bin"
else
  command_dir="$dest"
fi
printf 'Installed rundevrun and rundevtop %s in %s\n' "$version" "$dest"
case ":$PATH:" in
  *":$command_dir:"*) ;;
  *) printf 'Add this directory to PATH: %s\n' "$command_dir" ;;
esac
printf 'Existing supervisors keep running their current code. Restart affected services with rundevrun down and rundevrun up.\n'
