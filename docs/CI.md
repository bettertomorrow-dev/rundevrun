# CI and releases

The `CI` workflow runs on pull requests, pushes to `main`, and manual dispatch.
Only its final `CI` job should be required by branch protection. That job always
runs and checks every selected job, including failures and unexpected skips.

## Selecting checks

`scripts/ci-changes.py` compares the complete PR diff from its merge base, or the
before/after revisions for a push. Rename detection is disabled so both the old
and new paths participate. Unknown files, unavailable revisions, workflow
changes, and manual/release runs select every check.

- Root README, agent instructions, license, prose in `docs/` and `skills/`, and
  skill `agents/openai.yaml` metadata skip app checks.
- Go files, `go.mod`, `go.sum`, and executable example fixtures run Go checks.
- Embedded guide files run Go checks. Embedded JavaScript helpers also run Node tests.
- Skill/helper JavaScript runs the Node tests.
- Installer scripts and local Git hooks run installer tests on macOS and Linux.
- GoReleaser configuration runs Go and installer checks.

Go checks include formatting, module verification/tidiness, vet, release snapshot
archives for all four targets, race tests, and CLI/PTY scenarios on macOS/Linux.
The WSL job installs Ubuntu under WSL 2 on a Windows runner and runs race tests
and CLI/PTY scenarios on the Linux filesystem. It does not validate Windows
browser forwarding or live external providers.

Security checks use a pinned `govulncheck` on macOS and Linux. They run for Go
changes, releases, manual dispatch, and weekly even when source has not changed.
Dependabot proposes weekly Go module and action updates. Tool versions and action
commit revisions are pinned; update those alongside dependency maintenance.

To reproduce lightweight checks:

```sh
python3 scripts/test-ci-changes.py
python3 scripts/test-install.py
sh scripts/rdr-local-test
node --test scripts/domain-wizard-token.test.mjs
```

## Publish a release

Release publishing starts with a stable `vX.Y.Z` tag on a commit reachable from
`main`. The release workflow runs the entire CI suite against that exact tag.
Only after it succeeds does GoReleaser publish archives containing both binaries,
SHA-256 checksums, and GitHub-generated release notes. Both binaries report the
same product version; the public JSON API version is independent.

For the first release, after the implementation is merged and CI passes:

```sh
git switch main
git pull --ff-only origin main
git tag -a v0.1.0 -m "rundevrun v0.1.0"
git push origin v0.1.0
```

Use the next appropriate semantic version for subsequent releases. Ordinary
pushes and documentation edits never publish releases. Never move a published
release tag to a different commit.

Homebrew publication uses `TAP_GITHUB_TOKEN`, a repository Actions secret with
Contents read/write access to `bettertomorrow-dev/homebrew-tap`. The workflow's
normal `GITHUB_TOKEN` publishes assets in this repository. A missing tap token
produces a warning and skips Homebrew publication while still publishing GitHub
assets. Configure the token before the first release if Homebrew is required.
The generated cask installs both commands on macOS. Linux and WSL use the Bash
installer or the release archives.

The Bash installer shares the local updater's install directory and lock. It
leaves runtime state and running processes intact. Keep `main` publicly readable
for unauthenticated installation. Release and snapshot builds use GoReleaser
v2.20.2; reproduce packaging locally with:

```sh
TAP_GITHUB_TOKEN='' goreleaser release --snapshot --clean
```

## Publish guide updates

The CLI checks the protected `main` branch of `bettertomorrow-dev/rundevrun`
for guide updates. Compatible edits under `internal/guides/` become available on
the next check without a binary release. The manifest sets `guide_protocol` and
`min_cli_api_version`. Bump the guide protocol for backward-incompatible command
expectations, even when the CLI JSON envelope version stays the same. Set the
minimum CLI API version when the guide needs a newer API. Older clients retain
their last compatible download or use the bundled guide. Treat write access to
these files as release authority: guides can instruct agents to run scripts.
