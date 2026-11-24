@RTK.md

## Coding rules

- Write as few tests as possible. Add only the minimum tests needed to verify the changed behavior.
- In each source code file, write only one explanatory comment at the beginning describing the file's purpose. Keep the rest of the file free of comments, except required compiler or tool directives.

## Runtime state migrations

- Keep legacy detection, conversion, cleanup, and their unit tests in `internal/statemigration`. Entry points call the package and present its results. Keep shared operations in their owning packages.
- Use [maintain-migrations](.agents/skills/maintain-migrations/SKILL.md) when adding, updating, or removing a runtime state migration or reviewing one for a release.

## Publish a release

- When asked to publish a release, follow the [release instructions](docs/CI.md#publish-a-release). Push an annotated `vX.Y.Z` tag for the intended commit reachable from `origin/main`.
- Let the `Release` workflow publish the GitHub release and update `Casks/rundevrun.rb` in `bettertomorrow-dev/homebrew-tap`. Verify the workflow and the cask update for the tagged version.

## Local rundevrun installation

- Run `rtk proxy ./scripts/rdr-local update` from any worktree to build and install its current files, including uncommitted edits. Use `update --committed` to build only its committed HEAD. The previous binary is saved before replacement; build or smoke-check failures keep the installed binary.
- If an installed update breaks rundevrun, run `rtk proxy ./scripts/rdr-local rollback`, then `rtk proxy rundevrun version --json`. Rollback needs no Go build and preserves runtime state and registrations. Repeating rollback restores the same saved binary. A later successful update replaces that backup.
- Installation defaults to `~/.local/opt/rundevrun/rundevrun`, reached through `~/.local/bin/rundevrun` on this machine. `RDR_INSTALL_DIR` overrides the install directory for testing.
- Local hooks rebuild committed `main` after merges and rebase pulls on `main`. Enable them with `rtk git config --local core.hooksPath .githooks` after checking for existing custom hooks. Retry failed automatic updates on `main` with `rtk proxy ./scripts/rdr-local update --committed`.
- Existing supervisors retain their running code after update or rollback. Stop and start affected services with `rundevrun down` and `rundevrun up`, using the same project, worktree, and service selectors, when testing supervisor changes. This restarts the supervisor and its children. Replacing the binary alone does not restart services.
