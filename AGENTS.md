# Twinwright agent instructions

This file guides AI coding agents contributing to Twinwright. It applies to the
whole repository. Read any deeper `AGENTS.md` in a directory you change; those
files add local instructions. Direct user instructions take precedence.

## Project and source of truth

Twinwright is a Go system for testing autonomous AI agents against isolated,
stateful simulated worlds. Its contract is evidence-backed reliability, not
production integrations. Start with `README.md` for usage,
`CONTRIBUTING.md` for contribution rules, `SECURITY.md` and
`docs/security-review.md` for security boundaries, and `docs/adr/`
for architectural decisions. Do not treat historical ADRs as a replacement
for checking current code and tests.

- `cmd/twinwright/`: CLI entry points and demo.
- `internal/`: agent execution, world compilation, dispatch, persistence,
  workers, replay, assertions, tracing, and shadow comparison.
- `examples/`: versioned scenario, behavior, policy, and observation fixtures.
- `scripts/`: deterministic end-to-end and performance scripts.
- `.github/workflows/`: CI and optional integration gates.

## Working rules

- Use the Go version specified in `go.mod` (currently 1.27.3). Run
  commands from the repository root.
- Make the smallest change that solves the task. Follow existing Go patterns,
  run `gofmt`, and add a regression test for behavior changes. For a bug,
  show that its new test fails without the fix before claiming protection.
- Do not bypass, silence, or weaken a check to make a change look green.
  Report skipped checks and missing environment dependencies explicitly.
- Use scripted providers for deterministic tests; do not require real API keys,
  live model calls, internet access, Docker, or PostgreSQL in the default suite.
- Never commit credentials, local databases, built binaries, benchmark outputs,
  generated manifests, or logs. Do not run destructive cleanup on user data.
- For changes to guarantees listed below, document the architectural decision
  in `docs/adr/` and add adversarial coverage, not just a happy-path test.

## Invariants to preserve

- `world_id` isolates state. Tool calls are idempotent by call ID; reusing an ID
  with different arguments must fail.
- Local state changes and their ledger events commit atomically.
- Authorization happens in code inside the tool transaction, before handlers.
  Prompts, transcripts, and observed external text never grant permissions.
- Replay and inspection never mutate the source database.
- Distributed ownership and fencing guard durable writes in the same
  transaction. Delivery is **at-least-once**, not exactly-once.
- The event ledger records the agent trajectory, not worker identities,
  lease renewals, or ownership bookkeeping.
- Shadow mode is **observe-only** and reads confined, size-limited local
  records; never add a path that forwards observations to external services.

## Verification

Run the focused package test while iterating, then run:

```sh
make verify       # gofmt check, go vet, go build, unit tests, SQLite e2e
make test-race    # concurrency and fencing coverage
make bench-smoke  # one execution of each benchmark
```

With a running PostgreSQL service, use `make verify-full` for the
PostgreSQL and distributed integration gates. Real-container tests belong
in the Docker-enabled integration lane; see `.github/workflows/integration.yml`.
CI also runs gitleaks, govulncheck, module-tidy checks, and a narrated demo.

Before declaring success, state which commands ran, their outcomes, and what
could not be verified. Commits should explain *why*, not just what changed.
