# Internal package instructions

Applies to `internal/`. Follow root `AGENTS.md` as well.

- Keep package responsibilities separate: `compiler` describes worlds;
  `agent` records provider turns; `dispatch` enforces tool execution;
  `store` owns durable persistence; `worker` manages leases and retries.
- Do not introduce a second ledger, bypass the store's guarded write paths, or
  silently turn a failed operation into a successful observation.
- Error paths must preserve evidence and transaction rollback, especially for
  timeouts, canceled contexts, crash recovery, and ambiguous commits.
- Use existing clock, scripted provider, temporary SQLite, and fixture helpers
  for deterministic tests. PostgreSQL-only cases belong behind the existing
  `TWINWRIGHT_TEST_POSTGRES_DSN` test gate.
- Prefer a focused package test while editing; run `go test -count=1 ./...`
  and `go test -race -count=1 ./...` before final verification.
