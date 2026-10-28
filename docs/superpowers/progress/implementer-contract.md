# Implementer contract (applies to every task of the final milestone)

## Environment
- Work only in the checkout of branch `cloud/m2-platform`. Never edit `main` or any other branch. Never push (the coordinator pushes).
- Go version: whatever `go.mod` names (1.27.3 at the time of writing). Check with `go version`.
- Docker, PostgreSQL, Ollama and Python may or may not exist on the machine. Gated tests skip without them. Write them anyway.
- Scratch files go under `.superpowers/sdd/` (git-ignored), never in the repo tree.

## Required method: test-driven development
Write the failing test first. Run it and confirm it fails for the expected reason. Then write the minimal implementation and confirm it passes. Record RED and GREEN evidence in your report. For CI-only or docs-only steps, say what evidence exists instead.

## Binding constraints (from the plan's Global Constraints)
- No new Go module dependencies: `require` lines unchanged, and `go mod tidy` produces no diff.
- `go test ./...` stays hermetic: no network, API key, Docker, PostgreSQL, Ollama or Python requirement.
  - Opt-in env vars: `TWINWRIGHT_TEST_POSTGRES_DSN`, `TWINWRIGHT_TEST_DOCKER=1`, `TWINWRIGHT_TEST_OTEL=1`, `TWINWRIGHT_TEST_LIVE_OPENAI=1`, `TWINWRIGHT_TEST_LIVE_ANTHROPIC=1`.
  - Tests that need python3 skip when `exec.LookPath("python3")` fails.
- Before committing: `gofmt -l .` prints nothing, `go vet ./...` passes, and `go test -count=1 ./...` passes.
- Preserve the `AGENTS.md` invariants and ADR 0029's contract:
  - The ledger never records worker identity.
  - Delivery is at-least-once, never described as exactly-once.
- Existing CLI behaviour, flags, JSON fields and exit codes stay unchanged unless the brief says otherwise. New behaviour is opt-in.
- Secrets (API keys, configured secret values, `Authorization`, `x-api-key`) never reach the ledger, cassettes, traces, logs or reports.
- Match the repository's style: gofmt, doc comments that explain why, table-driven tests, existing helpers. Read the nearest `AGENTS.md` in any directory you change.
- Written text (comments, docs, commit messages):
  - no em dash character;
  - claims only what a test or CI job proves;
  - no mention of AI tools or assistants, and no `Co-authored-by` trailer.
  - Commit messages explain why.
  - Stage specific files, never `git add .`.
- Never weaken an invariant, skip or delete a test, or bypass hooks to get green. If blocked, report it with specifics.

## Scope
- Implement exactly what the task text in the plan specifies, and follow its file list.
- Do not restructure code outside the task.
- Note any file you had to touch outside the list, and why.

## Report
Write the report to `.superpowers/sdd/2026-10-27-final-milestone/task-N-report.md`. Cover:
- what you implemented;
- the tests you ran and their results;
- RED and GREEN evidence;
- the files changed;
- your concerns.

Reply with: status (DONE, DONE_WITH_CONCERNS, BLOCKED or NEEDS_CONTEXT), commits, a one-line test summary, and your concerns.
