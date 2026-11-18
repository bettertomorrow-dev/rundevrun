# Public repository audit

Audited on 2026-11-18. The repository remains private. Current-file cleanup is
prepared; publishing the existing repository still exposes historical content.

## Scope and results

- Inspected tracked docs, examples, source, tests, scripts, skills, and ignores.
- Gitleaks 8.30.1 found no secrets in the working tree or reachable local Git
  history using its default rules. No custom allowlist was used.
- Inspected all 554 unique reachable historical blobs across 91 commits for
  private project names, personal domains, filesystem paths, and HTTP hosts.
  Local history is not shallow; origin was fetched before final verification.
- Inspected GitHub repository metadata, all six PR descriptions, issue and PR
  comments, reviews, releases, Actions runs and artifacts, and remote branches.
  The only current remote branch is main. No comments, reviews, releases,
  Actions runs, or artifacts were present. Wiki and discussions are disabled.

Automated scanning cannot prove that no credential exists. Unreachable local
objects, other clones, deleted hosted content, and organization-level settings
are outside this audit.

## Cleanup prepared

Removed private project names, personal paths, and the personal domain from
current files. Replaced machine-specific HTTPS and acceptance diaries with
setup instructions and reproducible verification commands. Removed the old
PRD, implementation plan, review log, private migration map, and scenario that
required two private repositories. Kept technical contracts, user guides, and
self-contained synthetic examples. Test fixtures now use generic project names.

Added ignore rules for environment variants, private key containers, machine
networking state, generated setup plans, and local binaries. Ignore rules do
not protect files already committed or redact command output.

## Decisions before publication

1. Choose a publication history. Recommended: create a new public repository
   from the cleaned, reviewed snapshot with a fresh initial commit, retaining
   this private repository for its development history. Publishing this existing
   repository exposes private references in earlier file versions even after
   merging the cleanup. Preserving its history requires a coordinated rewrite
   of every published branch and tag and a fresh audit.
2. If publishing this existing GitHub repository, sanitize
   [PR #1](https://github.com/bettertomorrow-dev/rundevrun/pull/1). Its body names
   private projects and a personal domain. Rewriting Git history does not erase
   PR descriptions, old PR diffs, or hosted commit references. Verify these
   separately before changing visibility.
3. Decide whether the current owner identity may be public. `Better Tomorrow`
   remains in LICENSE; `bettertomorrow-dev` remains in the Go module path,
   imports, build script, and repository URL. Copyright attribution was retained.
   A rename requires coordinated import and build-path changes.
4. Existing commit metadata contains a personal name and personal email as well
   as a GitHub noreply address. A fresh public commit should use the intended
   public author identity. A history rewrite must address metadata too.
5. Confirm publication rights for code adapted from a private codebase.
   Removing its name from the source comment does not establish those rights.

A concise replacement for PR #1's description is:

> Implements rdr, a development-service runner across projects and existing Git
> worktrees. Includes strict configuration, process supervision, dependency
> bindings, worktree switching, loopback routing, and password-protected live
> and snapshot sharing. See the current verification guide for reproducible
> checks and platform limits.

## Validation of this cleanup

Passed `go vet ./...`, tests for `internal/monitor`, `internal/routing`, and
`internal/procid`, all six Node token-helper tests, `git diff --check`, and local
Markdown link checks. No runtime logic changed. Full lifecycle suites, external
provider scenarios, and Linux-specific tests were not rerun for this cleanup.
