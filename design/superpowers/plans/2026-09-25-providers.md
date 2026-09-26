# Milestone 10: providers plan

Spec: `design/superpowers/specs/2026-09-25-providers-design.md`. Worktree `.worktrees/milestone-10-providers` from `milestone-9-observability` (`6399a19`) while PR 8 is open.

## Task 1: provider allowlist

- Files: `internal/agent/provider.go`, fork validation, tests.
- `KnownProvider`. Fork accepts `anthropic` and `openai-compatible` with an explicit model, and still rejects unknown names.
- Commit: `Accept additional agent provider names`

## Task 2: chat completions adapter

- Files: `internal/agent/chat.go`, `chat_test.go`.
- Tests: tool call round trip, usage mapping, absent usage, redacted error, missing base URL.
- Commit: `Add OpenAI-compatible chat completions provider`

## Task 3: Anthropic adapter

- Files: `internal/agent/anthropic.go`, `anthropic_test.go`.
- Tests: tool use round trip, raw blocks preserved, usage, redacted error, missing key or model.
- Commit: `Add Anthropic messages provider`

## Task 4: CLI, README, ADR 0013

- Files: `cmd/twinwright/main.go`, tests, README, ADR.
- Preflight rejects a missing base URL or Anthropic key before creating a database.
- Commit: `Add provider CLI flags and documentation`
