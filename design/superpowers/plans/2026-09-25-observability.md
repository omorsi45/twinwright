# Milestone 9: observability plan

Spec: `design/superpowers/specs/2026-09-25-observability-design.md`. Worktree `.worktrees/milestone-9-observability`, branch `milestone-9-observability` from `milestone-8-counterfactual` (`09af1b0`) while PR 7 is open.

## Task 1: secret redaction and token usage

- Files: `internal/redact/redact.go` + test, `internal/agent/openai.go` + test.
- Tests: exact secret, `sk-` keys, `Bearer` tokens masked, ordinary text untouched; adapter HTTP 401 whose body echoes the key returns a redacted error and `RawBody`; usage parsed into `Message.Usage`; absent usage omitted.
- Commit: `Redact provider errors and record token usage`

## Task 2: ledger trace projection

- Files: `internal/trace/trace.go` + test.
- Interface: `Build(ctx, *store.Store, runID) (Trace, error)`; `Trace{TraceID, Summary, Root *Span}`.
- Tests: billing run with legacy 503, retry, and pause; chaos timeout-after-commit run; authorization denied; fork with observation override (fork event, parent link, no checkpoint flags); deterministic IDs across two builds; failed provider turn; redacted error text.
- Commit: `Build run traces from the event ledger`

## Task 3: CLI trace, OTLP export, inspect summary

- Files: `internal/trace/otlp.go`, `internal/trace/text.go` + tests, `cmd/twinwright/main.go` + test.
- Tests: OTLP structure (hex ID lengths, parent IDs, string times, typed attributes, fork link); text tree lines; `inspect` output starts with `summary` and keeps existing keys; unknown format rejected; missing database not created.
- Commit: `Add trace command and inspect summary`

## Task 4: documentation

- Files: `README.md`, `docs/adr/0012-ledger-traces.md`.
- Commit: `Document run traces and redaction`
