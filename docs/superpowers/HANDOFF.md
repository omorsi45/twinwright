# Handoff: resuming the Twinwright Cloud build

Updated 2026-10-27. This file and `docs/superpowers/progress/` hold everything needed to continue on any machine.

## What exists

- **Milestone 1, reliability core:** done and reviewed, on branch `cloud/m1-reliability-core` (head f06d0cb). CI and Integration are green. It covers crash boundaries, the crash matrix, real SIGKILL recovery, the delivery bound, sortable lease timestamps, leased local execution, and ADR 0029.
- **Final milestone:** branch `cloud/m2-platform`, stacked on milestone 1. This is the only remaining milestone. Milestones 2 to 7 of the design were merged into it at the owner's request, so merging this one branch delivers everything.
- **Design (binding):** `docs/superpowers/specs/2026-10-26-twinwright-cloud-design.md`, sections "Milestone 2" to "Milestone 7" and "Final milestone: scope decisions".
- **Plan (17 tasks):** `docs/superpowers/plans/2026-10-27-final-milestone.md`.
- **Progress and every decision made so far:** `docs/superpowers/progress/2026-10-27-final-milestone-ledger.md`. Milestone 1's record is `2026-10-26-m1-ledger.md`. Lines starting `Ruling:` are decisions taken on the owner's behalf.
- **Contract for every implementer:** `docs/superpowers/progress/implementer-contract.md`.

## Status of the 17 tasks

| Task | State |
|---|---|
| 1 Build metadata, report schema v2 | done |
| 2 Per-case fingerprint and trials | done |
| 3 The gate (`twinwright gate`) | done |
| 4 Regressed fixture, approved baseline, CI gate job, demo step 12, ADR 0030 | done; the CI `bench regression gate` job is green |
| 5 Provider config, response metadata, output limits, redaction | **in progress**: commit "Save interrupted provider-layer work" holds partial work (see below) |
| 6 to 17 | not started |

## Task 5 partial work: exact state

- `go build ./...` passes.
- `go vet ./...` fails: `cmd/twinwright/main_test.go` calls `providerFlags`, which does not exist yet.
- `internal/agent` and `internal/redact` tests pass.
- Inspect it with `git show 591a177` (subject "Save interrupted provider-layer work so it can resume elsewhere") and finish Task 5 from there.

Resolutions that already apply to Task 5:
- Move `selectProvider` to `cmd/twinwright/providers.go` as `selectProvider(cfg providerConfig)`. Preserve the `fixture-regressed-v1` branch and the `agent.WithManifest` binding at every call site: run, resume, fork, counterfactual, bench, shadow, worker.
- `providerConfig` gets only the fields needed now. Later tasks add theirs.
- Add `--max-output-tokens` without adding `--base-url` to a command that lacks it today.
- Anthropic keeps its 4096 default.
- Success-path redaction uses a new `redact.Secrets`, which masks only the configured key.
- The new `Message` fields are `omitempty`, so scripted ledgers and the committed baseline stay identical. The `fixture-v1` subtest of `TestRegressedBenchFailsTheGate` must still show zero warnings.

## Process for the rest (owner's explicit choice)

1. **Roles.** One coordinator plans and dispatches. Implementation is done by fresh implementer sessions, one at a time on this branch, never two at once. There are no per-task reviews: the owner's reviewer reviews the whole repository once everything is complete.
2. **Batches, to go faster:**
   - 5+6
   - 7
   - 8, then the coordinator's local live run (below)
   - 9+10
   - 11
   - 12+13
   - 14
   - 15, after the coordinator's perf runs (below)
   - 16+17, which also take the documentation minors listed in the ledger
3. **Each dispatch carries:**
   - one line of context;
   - the task sections copied from the plan, with exact values verbatim;
   - the contract;
   - the interfaces from earlier tasks (the plan's Interfaces blocks list them);
   - any ambiguity resolved;
   - the report path.
4. **After each batch:**
   - `git status` is clean.
   - Commit bodies contain no em dash and no `Co-authored-by`.
   - `make check && go test -count=1 ./...` is green.
   - If something is red, find the root cause, then dispatch a fix.
   - Append a ledger line: batch, commits, result, and any `Ruling: decision | why | cost if wrong`.
5. **No questions to the owner.** Decide and record. Stop only for a destructive action, a security-sensitive action, a merge, or a plan that is broken beyond guessing.

## Coordinator-only steps

- **Task 8.**
  - If Ollama is available, pull a small tool-calling model and run `scripts/live-model.sh`.
  - Dispatch a follow-up that writes `docs/live-model-run.md` from the report it produces, stating pass or fail honestly.
  - If no Ollama, use the CI `real-model` job's artifact instead and say where the run happened.
- **Task 15.**
  - Run `twinwright perf --trials 3 --out <file>` locally.
  - Dispatch the CI `perf` job (PostgreSQL) and download its artifact.
  - Hand both JSON paths to the implementer. Every figure in the docs must come from those files.
- **CI.** Push the branch (`git push origin cloud/m2-platform`) and verify the remote SHA. Dispatch both workflows:
  - `gh workflow run ci.yml -R omorsi45/twinwright --ref cloud/m2-platform`
  - `gh workflow run integration.yml -R omorsi45/twinwright --ref cloud/m2-platform`
  
  Docker-only evidence (PostgreSQL, containers, collector, Jaeger, Prometheus, the real-model job, demo-stack) comes from these runs unless the machine has Docker.

## Machine notes

- Use the Go version in `go.mod`.
- On Windows, run `make` and `scripts/*.sh` from Git Bash or WSL. The real-process crash test is unix-only and skips elsewhere.
- On the original Mac, `/usr/local/bin/git` was a broken x86 binary, and `PATH` needed `/usr/bin` first.

## Rules that never bend

- **Git:** never push to `main`, merge, force-push, or delete branches. Push only `cloud/m2-platform`.
- **Code:** no new Go module dependencies. Keep `go test ./...` hermetic. Preserve the `AGENTS.md` invariants and ADR 0029.
- **Secrets and data:** secrets never reach the ledger, cassettes, traces, logs or reports.
- **Writing:** no em dashes. State only claims a test or CI job proves. Report unfavourable results. Never mention AI tools or assistants in commits, docs or PRs.
- **Process:** never weaken or skip a test, and never bypass hooks.

## Finish line

1. All 17 tasks committed.
2. The fresh local gate is green: `make check && go test -count=1 ./... && go test -race -count=1 ./... && ./scripts/e2e.sh && make bench-smoke && make demo && go mod tidy && git diff --exit-code go.mod go.sum`.
3. The branch is pushed, and CI and Integration are green on its head. If `govulncheck` flags a new standard-library advisory, bump the `go` directive patch version, as was done for GO-2026-6809.
4. Report to the owner:
   - commits;
   - what each part delivers;
   - evidence (commands and CI run IDs);
   - the live-model and benchmark outcomes as measured;
   - every `Ruling:` line with its cost if wrong;
   - known limits.
5. Do not do the whole-branch review and do not merge. The owner's reviewer does that next.
