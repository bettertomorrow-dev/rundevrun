# Project setup

rundevrun supports two modes. Both use the same `rundevrun.toml` format and the same commands
from the project directory or its subdirectories.

Older setups used `dev.toml`. Rename that file to `rundevrun.toml`; its contents
stay the same. rundevrun no longer discovers `dev.toml` automatically.

## Shared

Create `rundevrun.toml` in the project root and commit it with the project. Each linked
worktree uses its own copy, so branches can change their service definitions.
See the [example](../README.md) and [configuration reference](TECH_SPEC.md).

## External

Keep personal runner config outside a Git repo. Write a `rundevrun.toml` proposal
somewhere outside the checkout, then run from the repo:

```sh
rundevrun config external --file /tmp/my-project-rundevrun.toml --dry-run --json
rundevrun config external --file /tmp/my-project-rundevrun.toml --json
rundevrun config check
rundevrun register .
rundevrun up api --dry-run
rundevrun up api
rundevtop
```

Replace `api` with your configured service names. Import and registration run
no project commands. Registration lets rundevtop recognize the project before its
first launch. `up` also registers automatically.

The external file lives at
`$XDG_CONFIG_HOME/rundevrun/projects/<project-id>/rundevrun.toml`, or
`~/.config/rundevrun/projects/<project-id>/rundevrun.toml` when `XDG_CONFIG_HOME` is unset.
`rundevrun config external --json` prints this location without creating anything;
it also accepts a repo path, such as `rundevrun config external /path/to/repo --json`.
Import validates against the selected worktree and refuses to overwrite an
existing external config. Edit that file directly for later changes.

The external file's presence selects External mode for the repo and every linked
worktree, including newly created ones. It takes precedence over any repo
`rundevrun.toml`; files are never merged. Invalid external config reports an error
instead of falling back. `rundevrun config check` reports the selected source and root.
Relative service paths resolve against the selected worktree, with the same
directory and symlink confinement checks as Shared mode.

Association uses the canonical Git common directory, matching rundevrun's project ID.
Separate clones have separate configurations even when their remotes match.
Changing the remote has no effect. Moving the Git common directory changes the
ID; reimport the saved config at the repo's new location.

To return to Shared mode, move the external file aside, preserving it for later,
and create or restore the repo's `rundevrun.toml`. Existing running services keep their
saved config until restarted.

External onboarding adds no runner files to the repo and changes no Git settings.
Services still run the configured commands, which may write their usual build
artifacts to the checkout. Runtime state and logs remain in rundevrun's state directory.

## Env files in worktrees

To share env defaults with new worktrees, add the filenames you want to opt in:

```toml
[project]
name = "example"
env_files = [".env", ".env.local"]
```

This works in Shared and External modes. Keep your env files in the main Git
checkout. New worktrees inherit their values automatically. To change a value
for one worktree, put just that value in its own matching env file. Worktree
values win over all main-checkout values; within each checkout, later files win.
Missing files are fine. No files or symlinks are created.

Paths are relative to the checkout root, even for services with another `cwd`.
Explicit service `env` values override file values. Run `rundevrun config check`
to validate your selection, and restart running services after env edits.
See the [env-file reference](TECH_SPEC.md#shared-env-files-across-worktrees)
for parsing and precedence details.

## Agent-assisted onboarding

Ask an agent to run `rundevrun guide onboard` from the checkout and follow the
returned instructions. No skill installation is required. The command reports
the active config and returns a guide without writing project config or
registering the project. The agent inspects existing commands, validates a launch
plan, and registers the project. Starting services requires a separate request.
The guide also covers re-onboarding, env files, fixed ports, and singleton services.

The CLI checks for compatible guide updates at most once per 24 hours. Run
`rundevrun guide onboard --no-update` to skip the network check. The result includes
`guide_dir`, `source`, and `revision`. To freeze a guide, copy the reported
`guide_dir` to `$XDG_CONFIG_HOME/rundevrun/guides/onboard.local/`, or
`~/.config/rundevrun/guides/onboard.local/` if `XDG_CONFIG_HOME` is unset.
Copy the whole directory, including its entry point and supporting files. The CLI
prefers this user-owned copy while it continues checking for updates. Remove the
local copy to use the downloaded or bundled guide again.

## Fixed auth URLs and one active worktree

Some apps require fixed URLs because their auth callbacks or cross-app links
are registered as `http://localhost:8010` or `http://localhost:8011`. Keep those
URLs and ports when the backend's redirect support has not been changed.
For each service that must run in only one linked worktree, add:

```toml
[services.web]
command = "existing-development-command"
port = 8010
singleton = true
```

Use the app's supported command flags or environment to bind the configured
port on loopback. Keep its existing `NEXTAUTH_URL` and cross-app URLs intact;
the setting does not rewrite them. Use the accepted localhost URL for login
rather than substituting a routed hostname.

Starting `web` in another linked worktree automatically prepares it, stops the
previous instance, and starts the target on the same port. This causes a brief
interruption. Preview the handoff with `rundevrun up web --dry-run --json`.
Separate clones remain independent projects, and unmanaged port owners remain
conflicts. See [singleton behavior](TECH_SPEC.md#singleton-services) for failure
and recovery details.
