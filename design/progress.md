# Twinwright development progress

**As of 2026-10-02.** This file is a dated snapshot, not a source of truth. The
truth is the repository: `git log`, the open pull requests, `CHANGELOG.md` and
`docs/adr/`. When this file and the repository disagree, the repository is right and
this file is stale.

It used to be a per-milestone narrative of tasks, commits and review notes. That
duplicated git history while rotting faster than it: the version this replaced still
described `main` as being at PR 13, still pointed at a checkout path that no longer
exists, and claimed no open PRs while three were open. A narrative nobody can verify
in one command is worse than no narrative, so what is left here is only what a
reader cannot get from `git log` in less effort than it takes to read.

## Where main is

`main` at `2334811`, the merge of PR 22. PRs 20, 21 and 22 were the last three to
land: honest bench aggregation, the flagship suite, and the distributed documentation
plus the cut `v0.1.0` changelog section.

Checkout for this work: `C:/Users/ermaomo/Personal/github/twinwright`.

## What is open

Three pull requests, one feature each, all based on `main`:

- **23**, `shadow-comparison-depth`: semantic argument divergence, order divergence
  and a policy screen in the shadow comparison. ADR 0026.
- **24**, `provider-timeout-and-truncation`: a per-attempt provider deadline on the
  request context, truncated completions refused as decisions, and the interrupted
  attempt recorded as a replayable event. ADR 0027.
- **25**, `documented-security-review`: `docs/security-review.md` plus the test that
  fails CI when its citations drift. ADR 0028.

Nothing else is in flight. The roadmap in
`briefs/2026-09-25-platform-roadmap.md` is direction, not a claim about what is
implemented.

## Not tagged

`CHANGELOG.md` has a cut `v0.1.0` section and the repository has no tags. Tagging is
a decision rather than a chore: a tag is awkward to retract once anyone has fetched
it, so it waits for an explicit call.

## How to check this yourself

```bash
go build ./...
go vet ./...
go test ./... -count=1              # 26 packages
go run ./cmd/twinwright demo --dir "$(mktemp -d)/demo"   # eleven steps, each verifying its own claim
./scripts/e2e.sh                   # the documented pipeline, end to end
```

On this Windows host, `go env GOTOOLCHAIN` is pinned below what `go.mod` requires:
export `GOTOOLCHAIN=go1.27.1` per shell. Git Bash here has no Go on `PATH`, so a
script that shells out to `go` needs `PATH="/c/Program Files/Go/bin:$PATH"` exported
inside it.

## Limits worth carrying forward

These are stated in the README, `SECURITY.md`, `docs/security-review.md` and the
relevant ADRs. They are listed here because each one is a thing a reader could
otherwise assume has been verified.

- **No live provider call has ever been verified.** There is no API key on this
  host. Every provider behaviour is held against an `httptest` server.
- **The Mermaid diagrams are checked by structural rules**, in
  `internal/store/diagrams_test.go`, not by a real parser. GitHub renders them
  natively, so the rendered page is the render evidence.
- **A read-only SQLite open still creates `-wal` and `-shm` sidecars** and therefore
  fails in a non-writable directory. Shared by `replay`, `evaluate` and `trace`, and
  documented in the README.
- **The container tests that touch a real daemon are skipped** unless
  `TWINWRIGHT_TEST_DOCKER=1`, and the PostgreSQL parity tests are skipped without a
  server DSN. A green default run is not evidence about either.
- **`go test -race` is run by CI, not on this host.**

## This folder

`design/README.md` describes what else is in here. Everything under
`design/superpowers/` is a working artefact from an earlier phase that was committed
before the convention changed; new specs and plans stay local and uncommitted.
Public architectural decisions belong in `docs/adr/`.
