---
name: rdr-onboard
description: Onboard a repository to rundevrun using a shared rundevrun.toml or an external personal config, including repos where runner files are forbidden.
---

# Onboard a repo to rundevrun

Configure the installed `rundevrun` for the repository's existing development commands.
Use `rundevrun --help` to check available commands. If `config external` is unavailable,
report that External mode needs a newer rundevrun before changing files.

## Choose the mode

Honor the mode already requested. Otherwise ask whether the team adopts rundevrun
or the setup must stay outside the repo.

- **Shared:** create or update `rundevrun.toml` at the project root for the team to commit.
- **External:** keep runner configuration under rundevrun's user config directory.
  Leave the checkout, Git metadata, package scripts, ignore files, hooks, and
  agent instructions unchanged. Service commands may still write their normal
  build output when the user runs them.

These are the only two modes. A locally ignored `rundevrun.toml` is still a repo file.

## Inspect and configure

Read existing run instructions, manifests, environment examples, and service
entry points. Reuse the project's commands and dependency tooling. Identify
which services are needed, their working directories, dependency order, and
how they accept a loopback host and a port. Check existing runner configuration
before replacing anything.

Use the same version 1 TOML format in either mode:

```toml
version = 1
[project]
name = "example"
[services.worker]
command = "existing-development-command"
cwd = "."
```

Service names use lowercase letters, digits, underscores, and hyphens, beginning
with a letter. `cwd` is relative to the selected worktree and must exist there.
Omit `cwd` for the root. Services without ports default to `process`.
For an HTTP service, use `port = "auto"`, pass `{host}` and `{port}` to the
command through the app's supported flags or environment, and add an existing
health endpoint if available. Commands place rundevrun placeholders outside shell
quotes. Use `depends_on = ["service-name"]` for actual startup dependencies.
Keep credentials in the project's existing environment setup.

For Shared mode, write `rundevrun.toml` in the repo. First run
`rundevrun config external --json` in a Git repo to locate any existing external config.
If that file exists, it currently takes precedence. Explain the conflict and
preserve its contents when moving it aside to activate the requested Shared mode.

For External mode, write the proposed TOML to a temporary file outside the repo,
then run these commands from the repo:

```sh
rundevrun config external --file /absolute/path/to/proposed.toml --dry-run --json
rundevrun config external --file /absolute/path/to/proposed.toml --json
```

The result gives `config_file` and `worktree_root`. Import refuses to overwrite
an existing external config. To update one, inspect and edit the returned file
directly, preserving unrelated services. External config applies to all linked
worktrees and takes precedence over their `rundevrun.toml` files. Separate clones have
separate configs. Remove the temporary proposal after importing it.

## Verify and register

Run `rundevrun config check --json` and inspect `config_mode`, `config_file`, and
`worktree_root`. Run `rundevrun up SERVICE... --dry-run --json` for the intended services.
Fix validation or planning errors before registering with `rundevrun register . --json`.
Registration runs no project commands and lets rundevtop recognize the repo.

Start services only when starting them is in the user's requested scope.
When authorized, use `rundevrun up SERVICE... --json`, inspect status and logs, and
resolve startup errors. Otherwise finish with the validated config and commands
to start services and open `rundevtop`.

Report the mode, config path, configured services, validation result, and any
environment setup still needed. In External mode, verify onboarding left repo
files and Git metadata unchanged, accounting for changes present before the task.
