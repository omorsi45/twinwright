# Twinwright: continue from Milestone 10 (WIP)

You are the principal engineer continuing Twinwright, an open-source Go/SQLite platform that tests AI agents inside executable, stateful simulations of company software (billing, CRM, ticketing, messaging) without touching production systems.

Milestones 1 through 9 are merged into `main`. Milestone 10 (multiple agent providers) was started in a worktree and then abandoned mid-implementation with **uncommitted** changes. Your job is to finish Milestone 10 cleanly, then keep going through the roadmap (Milestones 11 to 14) using the same workflow, stopping only for the blockers listed at the end.

Work autonomously. The owner (Omar) has repeatedly said: do not ask design questions, take the recommended option and keep going. Report progress in short plain sentences. Never use em dashes.

## Current GitHub and local state (verified 2026-09-25)

| PR | Title | Status | Merge commit |
| --- | --- | --- | --- |
| 1–6 | Milestones 2–7 | MERGED | earlier |
| 7 | Counterfactual intervention analysis (M8) | MERGED | `36c32ef` |
| 8 | Ledger traces and inspect summaries (M9) | MERGED | `44d25e1` |

- Remote `origin/main` tip: `44d25e1` (PR 8 merge). Local main checkout may be behind: run `git pull --ff-only origin main` first.
- Public ADRs through `docs/adr/0012-ledger-traces.md`. Next ADR number is **0013**.
- Open PRs: none.
- Worktrees currently present:
  - `.worktrees/milestone-8-counterfactual` at `09af1b0` (merged; removable)
  - `.worktrees/milestone-9-observability` at `6399a19` (merged; removable)
  - `.worktrees/milestone-10-providers` at `6399a19` with **dirty uncommitted WIP** (this is your starting point)

After confirming branches are merged and clean, remove the M8 and M9 worktrees with `git worktree remove`. Do not delete the M10 worktree until you decide how to recover the WIP (see below).

## Locations and tools

- Main checkout: `C:\Users\Omar Morsi\Desktop\Projects\twinwright` (branch `main`). GitHub: https://github.com/omorsi45/twinwright (public, Apache-2.0).
- Go is NOT on PATH. Use `C:\Users\Omar Morsi\Desktop\Projects\twinwright\.tools\go\bin\go.exe` and `...\gofmt.exe` (Go 1.27.1, ignored `.tools/` folder). CGO is off, so `go test -race` is unavailable.
- Shell is Windows PowerShell 5.1. `gh` is authenticated as `omorsi45`. `gitleaks` is on PATH; the repo has a pre-commit hook that runs it. Do not use `--no-verify`. Fake API-key fixtures that look like real keys will fail the hook; assemble them at runtime in tests (see M9 Task 1).
- Private design workspace (Git-ignored, never commit it): `C:\Users\Omar Morsi\Desktop\Projects\twinwright\design\`
  - `design/README.md`: index. Read it first.
  - `design/briefs/2026-09-25-platform-roadmap.md`: source of truth for scope. Milestone 10 ~978, 11 ~999 (benchmark runner ~1060), 12 ~1097, 13 ~1116, 14 ~1133. Also read Autonomous Execution, Mandatory Development Loop, Existing Invariants, TESTING STANDARD, EVENT MODEL, SECURITY, README REQUIREMENTS, COMMIT DISCIPLINE.
  - `design/progress.md`: continuity ledger. Append after every task. It may still say PR 8 is open; correct that when you update it.
  - `design/superpowers/specs/2026-09-25-providers-design.md` and `plans/2026-09-25-providers.md`: already written for M10. Follow them.
  - Specs/plans for M8 and M9 are also there and marked completed.
- Obsidian vault: `C:\Users\Omar Morsi\Desktop\Obsidian\Claude`
  - Read `wiki/projects/twinwright/overview.md` and `wiki/projects/twinwright/conversation-log.md` before starting.
  - After vault writes: append to `log.md`, keep `updated:` frontmatter current, update the Twinwright line in `index.md` when status changes.
  - ENCODING WARNING: never use PowerShell `Get-Content | Set-Content` or `Set-Content -Encoding utf8` on vault or repo text files. Use file edit tools, `Add-Content` (append only), or Python with explicit UTF-8 without BOM.

## What already shipped on main (do not re-implement)

Packages under `internal/` (plus CLI):

- `compiler`, `behavior`, `store`, `dispatch`, `agent` (OpenAI Responses + scripted fixtures), `chaos`, `authz`, `eval`, `replay`, `checkpoint`, `fork`, `assertion`
- `counterfactual` (M8): intervention analysis; `fork.Options.Observation` for audited tool-response overrides; CLI `counterfactual`
- `trace` (M9): ledger-derived span tree; CLI `trace` (`json`/`text`/`otlp`); `inspect` leads with `summary`; `redact` for secrets; OpenAI adapter records `Message.Usage` and redacts error bodies
- CLI: `build`, `build-world`, `run`, `resume`, `checkpoints`, `fork`, `compare`, `replay`, `evaluate`, `counterfactual`, `inspect`, `trace`

Public docs: README (Omar's structure; add sections, do not restructure), ADRs 0001–0012.

## Milestone 10 WIP (recover this first)

Branch/worktree: `.worktrees/milestone-10-providers` at `6399a19` (same tip as merged M9). **No M10 commits yet.** Dirty tree as of handoff:

Tracked modifications:
- `cmd/twinwright/main.go` (+ CLI agent names, `--base-url`, `selectProvider` cases for `openai-compatible` and `anthropic`)
- `cmd/twinwright/main_test.go`
- `internal/agent/openai.go` (likely extraction toward shared helper)
- `internal/fork/fork.go`, `internal/fork/fork_test.go` (allowlist)

Untracked (new files already drafted):
- `internal/agent/provider.go` (~26 lines, `KnownProvider`)
- `internal/agent/chat.go` + `chat_test.go` (OpenAI-compatible chat completions)
- `internal/agent/anthropic.go` + `anthropic_test.go` (Messages API)

Missing relative to the plan: README section, ADR 0013, full gate commits, independent review, PR.

### Recommended recovery

1. `git pull --ff-only origin main` in the main checkout.
2. Inspect the dirty M10 worktree. Run focused tests on the new packages. Decide whether the uncommitted code is sound enough to keep.
3. Prefer: rebase/reset the branch onto current `origin/main` (`44d25e1`) while keeping the WIP, then finish with TDD per the plan (allowlist commit, chat commit, anthropic commit, CLI/docs commit). If the WIP is messy, stash or copy the new files aside, recreate a clean worktree from `origin/main`, and re-apply carefully.
4. Do not open a PR against an unmerged base; PR 8 is already on main, so target `main`.
5. After M10 ships, continue with Milestone 11 (Twinwright Bench) from the roadmap.

## Invariants you must not weaken

World isolation (`world_id` on every world query); deterministic seeding; call-ID idempotency; atomic local mutation; evaluation independent of execution; replay and evaluation never mutate the source database; runs without chaos/auth produce identical ledgers; old databases stay readable read-only; honest capability boundaries; model output recorded but never assumed deterministic; secrets never logged (use `internal/redact`).

## Mandatory workflow per milestone

Same as prior milestones:

1. Read roadmap section, `design/progress.md`, and relevant code.
2. Private spec + task plan under `design/superpowers/` (M10 already has both); index in `design/README.md`.
3. Isolated worktree from current `origin/main` (or recover M10 as above). Baseline `go test -count=1 ./...`.
4. TDD per task: failing test first, implement, focused tests, full gate.
5. Full gate before every commit: `gofmt -w` on changed Go files, `go vet ./...`, `go test -count=1 ./...`, `git add -A`, `git diff --cached --check`, commit, `gitleaks git --log-opts="-1" --no-banner .`. A gate script may exist at `$env:TEMP\tw_gate9.ps1` (hard-coded worktree path; update it). Make the script fail if `git commit` fails (gitleaks pre-commit can block silently otherwise).
6. Append to `design/progress.md` after each task.
7. After last task: build CLI to a temp folder, smoke-test real commands, independent read-only review of `git diff <base>..HEAD` via subagent (severity, file:line, failing scenario, fix). Fix every critical/important finding with a red-green regression test.
8. Update README (Omar's style: add a section; capability bullet; layout; ADR list) and add ADR. No em dashes.
9. Fetch, `git merge-tree --write-tree HEAD origin/main`, gitleaks on `origin/main..HEAD`, push, `gh pr create --body-file` with Summary / Verification / Limits.
10. NEVER merge the PR yourself. Update design + vault, then start the next milestone.

GitHub rule: never mention Claude, AI, Codex, agents writing code, or any assistant in commits, PR titles/bodies, comments, or issues.

Code style: match surrounding code. Small composable functions. Every changed line traces to the task. Note unrelated issues in the report/`progress.md` instead of fixing silently.

## Roadmap remaining after M10

- **Milestone 11:** Twinwright Bench (serious scenario suite + runner + JSON + compare). Do not invent 1000 trivial templates.
- **Milestone 12:** Shadow mode architecture (observe only by default; no production writes without explicit config). Experimental and labeled.
- **Milestone 13:** Optional containerized world execution (Docker when isolation actually needs it; not Kubernetes for resume keywords).
- **Milestone 14:** Distributed runtime only after single-node semantics are solid; define consistency/idempotency/leases before claiming anything.

## Open items and known limits to carry forward

- Read-only opens create `-wal`/`-shm` (SQLite WAL). Affects `replay`, `evaluate`, `trace`, `inspect` in non-writable directories. Documented; needs a store-level decision as its own small PR if fixed.
- No live OpenAI / Anthropic / local-server run has been verified (no keys). Keep scripted fixtures as the tested path; say so honestly.
- Security classification in `eval.AnalyzeSecurity` uses call targets only; customer scope does not restrict messaging.
- Forks of forks unsupported.
- Missing-database / unknown-run CLI errors are still unclear (`out of memory (14)`, `sql: no rows`) across several commands; pre-existing; fix only if you take it as a small dedicated change.
- `--steps` has no upper bound on `run` / `fork` / `counterfactual` (consistent; leave alone unless changing all).

## When to stop and ask

Only: missing credentials that block the task itself, unrecoverable contradiction, destructive data risk, or a decision that truly needs information you cannot get. Otherwise continue. When you finish a milestone, give Omar: what shipped, PR link, what was verified, what review found and fixed, anything he must decide.
