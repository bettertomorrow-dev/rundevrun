@RTK.md

## Coding rules

- Write as few tests as possible. Add only the minimum tests needed to verify the changed behavior.
- In each source code file, write only one explanatory comment at the beginning describing the file's purpose. Keep the rest of the file free of comments, except required compiler or tool directives.

## Local rdr installation

- Run `rtk proxy ./scripts/rdr-local update` to build and install committed `main`. Uncommitted edits are excluded. The previous binary is saved before replacement; build or smoke-check failures keep the installed binary.
- If an installed update breaks rdr, run `rtk proxy ./scripts/rdr-local rollback`, then `rtk proxy rdr version --json`. Rollback needs no Go build and preserves runtime state and registrations. Repeating rollback restores the same saved binary. A later successful update replaces that backup.
- Installation defaults to `~/.local/opt/rundevrun/rdr`, reached through `~/.local/bin/rdr` on this machine. `RDR_INSTALL_DIR` overrides the install directory for testing.
- Local hooks rebuild after merges and rebase pulls on `main`. Enable them with `rtk git config --local core.hooksPath .githooks` after checking for existing custom hooks. Retry failed automatic updates with the update command above.
- Existing supervisors retain their running code after update or rollback. Stop and start affected services with `rdr down` and `rdr up`, using the same project, worktree, and service selectors, when testing supervisor changes. This restarts the supervisor and its children. Replacing the binary alone does not restart services.
