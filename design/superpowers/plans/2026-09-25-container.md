# Milestone 13: optional containerized world execution plan

Spec: `design/superpowers/specs/2026-09-25-container-and-distributed-design.md` (M13 section).
Worktree: `.worktrees/milestone-13-container` from `origin/main` (`2c46166`).

## Task 1: container package

- Files: `internal/container/{config,executor,docker,local}.go` + tests.
- Parse version 1 sidecar YAML. `Local` is a no-op. `Docker` uses an injectable runner; missing daemon returns a clear error only when Docker runtime is selected.
- Commit: `Add optional container executor for world sidecars`

## Task 2: CLI, example, docs

- Files: CLI `container`, example config, README, ADR 0016.
- Commit: `Expose optional twinwright container commands`

Milestone 14 stays design/ADR-only in a follow-up after this PR.
