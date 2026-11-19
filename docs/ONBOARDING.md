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

Install [rdr-onboard](../skills/rdr-onboard/SKILL.md) in your agent's personal skills
directory, outside the target repo. Ask it to onboard a repo in Shared or External
mode. It inspects existing commands, writes the config, validates a launch plan,
and registers the project. Starting services is a separate action unless included
in your request.
The skill also detects env files. It opts in a sole candidate and asks which
files to use, and in what order, when several are present. Templates such as
`.env.example` are excluded.
