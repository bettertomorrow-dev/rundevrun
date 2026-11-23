---
name: maintain-migrations
description: Maintain rundevrun runtime state migrations. Use when adding or updating a migration, checking upgrade compatibility before a release, or removing a completed migration. Covers rundevrun's own runtime state.
---

# Maintain runtime state migrations

Use this repository skill from a rundevrun checkout.

1. Read [AGENTS.md](../../../AGENTS.md), `internal/statemigration`, and the runtime state section in [INSTALL.md](../../../docs/INSTALL.md). Search for migration calls in the CLI and rundevtop, their tests, and user-facing instructions.
2. Identify the supported old state and the first release that can migrate it. Check tags and release history. Use confirmed upgrade information for retirement decisions. A release tag proves availability. Completion on a machine requires separate evidence.
3. Keep changes in `internal/statemigration`. For changes that stop processes or delete state, verify ownership, stop the old processes, and persist the data being carried forward before cleanup. Preserve a safe retry after interruption.
4. Review the existing legacy directory migration after its transition release. Remove it once the installations with old state have completed the move. If a supported installation can skip that release, keep the migration and report what still needs to move.
5. When retiring a migration, remove its package code, obsolete entry-point calls, commands, help, and tests for the retired path. Remove stale upgrade instructions from the installation guide and the distributed skill. Keep shared operations that still have callers.
6. Verify changes with `rtk proxy go test ./...`. Exercise changed migration paths with temporary runtime directories. Report which migrations remain and the condition for removing each one.
