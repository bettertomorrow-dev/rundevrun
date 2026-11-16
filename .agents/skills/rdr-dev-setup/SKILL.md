---
name: rdr-dev-setup
description: Explain or set up a local rundevrun development installation with zsh toggles for the binary and isolated runtime state. Use when developing rdr alongside an installed version used by other projects.
---

# Local rdr development installation

Offer a terminal-scoped toggle between the installed `rdr` and a binary built
from this checkout. For an explanation request, describe the setup and commands.
Perform installation when the user asks to set it up; invoking this skill alone
is not an instruction to change shell configuration.

## Design

- Build the development binary at `~/.local/opt/rundevrun-dev/bin/rdr`.
- `rdr-dev-on` prepends that directory to PATH and exports
  `RDR_HOME=~/.local/state/rundevrun-dev`.
- `rdr-dev-off` restores the PATH and RDR_HOME captured on activation, including
  an originally unset RDR_HOME. Repeated on/off calls are harmless.
- Existing terminals keep their environment. Child processes inherit the active
  PATH and RDR_HOME. Switching does not replace or stop running services.
- The development state has its own project registrations. Register a project
  there only as part of the requested project setup.
- This isolates runner state, not application data or fixed ports. Project
  configurations still determine those resources.

Keep the installed binary intact. Its updates are independent of the development
build. A missing installed binary is worth reporting, but does not block setup.

## Setup

1. Locate the rundevrun checkout and read `docs/INSTALL.md` and `go.mod` for build
   requirements. Inspect `command -v rdr` before activation. Detect any shell
   alias or function that would take precedence over PATH.
2. Find the machine-local zsh configuration. Follow existing symlinks and source
   statements. In a dotfiles repository, verify the destination with
   `git check-ignore -v`. Prefer the existing ignored `configs/.zshrc.local`
   convention and its `~/.zshrc.local` symlink when present. Keep machine-specific
   paths and activation out of tracked dotfiles. If no local include exists,
   explain the required include before changing tracked shell configuration.
3. From the selected checkout, build:

   ```sh
   mkdir -p "$HOME/.local/opt/rundevrun-dev/bin"
   go build -o "$HOME/.local/opt/rundevrun-dev/bin/rdr" ./cmd/rdr
   ```

4. Merge the functions from [assets/rdr-dev.zsh](assets/rdr-dev.zsh) into the
   local zsh configuration. Preserve unrelated content; update existing
   definitions instead of appending duplicates. Create a missing symlink only
   after checking that its path is unused. Loading the file defines the toggles
   without activating development mode.
5. Verify in a fresh `zsh -f` that sources the local file: activation selects the
   development binary; `rdr version --json` succeeds; a child shell inherits the
   selection; repeated activation preserves the original environment; disabling
   restores PATH and both set and unset RDR_HOME cases. Verify that the local
   configuration remains ignored by Git.

## Handoff

Give the actual file and binary paths, then show:

```zsh
source ~/.zshrc.local
rdr-dev-on
rdr version --json
rdr-dev-off
```

Use the actual local include path if it differs. The agent's shell cannot
activate the user's existing terminal. New terminals load the function
definitions through the configured include.

Show the rebuild command with the actual checkout path. Rebuilding replaces the
shared development binary, so terminals using this setup use the latest build
on their next invocation. Already running services retain their processes.

Mention that disabling restores the entire saved PATH, so PATH edits made while
active are discarded. Treat installation of the stable version and configuration
of individual projects as separate tasks unless the user requested them too.
