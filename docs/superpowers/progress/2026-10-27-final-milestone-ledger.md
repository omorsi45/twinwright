<!-- Working record of the build process, kept in the repository so the work can resume on another machine. -->
# SDD ledger - plan: docs/superpowers/plans/2026-10-27-final-milestone.md

Spec: docs/superpowers/specs/2026-10-26-twinwright-cloud-design.md (Milestones 2-7 and "Final milestone: scope decisions" binding).
Worktree: .worktrees/cloud-m2-platform, branch cloud/m2-platform (stacked on cloud/m1-reliability-core f06d0cb). Plan commit ef62dda. Baseline: go test ./... 28 packages ok.
Models: implementers the implementation model; reviewers the review model (a lighter tier for small diffs and scoped re-reviews).

## Pre-flight scan

| Pair / task | Produces vs consumes | Finding |
|---|---|---|
| T1-T2 | both edit bench report.go/run.go/suite.go; T2 uses T1 fields | sequential, consistent |
| T1/T2-T3 | gate consumes SchemaVersion, ContentDigest, Trials, Passes, Mutations, SuccessfulViolations, Trajectory | consistent |
| T3-T4 | T4 uses gate CLI exit codes and baseline | consistent |
| T4-T5 | T4 adds a fixture-regressed-v1 branch to selectProvider; T5 moves selectProvider to providers.go with providerConfig | T5 must preserve the regressed branch (carry in dispatch) |
| T5-T7, T5-T12, T5-T13 | providerConfig fields Cassette/RecordCassette, AgentCommand/AgentEnv, AgentContainer | consistent |
| T6-T7 | retries recorded on turns; cassette replays Retry-After header | consistent |
| T9-T10 | both edit worker.go and distributed.go | sequential, consistent |
| T9-T11 | T11 asserts twinwright.event.start_seq from T9 | consistent |
| T1-T14 | perf Report embeds bench.Environment | consistent |
| T14-T15 | T15 needs controller-run perf JSON (Mac) and CI perf artifact | controller step between tasks |
| T8 | needs controller to pull a model and run scripts/live-model.sh locally | controller step after implementer |
| T4/T16/T17 | demo 12 steps (T4) and make demo 12/12 (T16) | consistent |
| T12-T13 | ExecProvider.Wrap consumed by ContainerOptions.Wrap | consistent |
| each task self | tests named, commands given, files listed | no contradictions found |

Ruling: if any task after T4 changes the standard suite's scripted results, the baseline file must be regenerated in that task and the reason stated in its commit - the gate would otherwise flag the change - cost if wrong: a CI gate failure that names the drift.
Ruling: CI-only evidence (Integration jobs real-model, observability, docker agent container, perf, demo-stack; PostgreSQL) is gathered by pushing the branch and dispatching workflows before finishing - the Mac has no Docker - cost if wrong: none beyond CI minutes.

## Progress
Task 1: implementer f1-impl DONE, commit 329ce7c (BASE ef62dda). Review f1-review: spec ✅, Approved.
Task 1: minor (deferred): ContentDigest field comment should say world/scenario fixtures are not covered; Run with empty Build gives empty version fields; schema v2 docs belong in ADR 0030 (Task 4); hash framing has no length prefix (plan-mandated, harmless).
Task 1: complete (commits ef62dda..329ce7c, review clean)
Task 2: implementer f2-impl DONE_WITH_CONCERNS, commit eaaf292 (BASE 329ce7c). Controller verified the main clone is clean at 0a61188 (stray edit reverted, nothing committed). AnalyzeSecurity used for violations (accepted).
Ruling: a multi-trial case's WallMS is the first trial's, not the sum, matching "trajectory, mutations and run ID come from the first trial" in the brief - summing inflates the suite's median latency by the trial count - cost if wrong: latency for multi-trial cases reflects one trial only.
Task 2: follow-up f2-fix DONE 211c648. Review f2-review: spec ❌ (trial-1 duplicate-effect inputs), Needs fixes (2 Important). ⚠️ main clone confirmed clean by controller.
Ruling: the multi-trial fold is a pure function with order-independent precedence: Status failed when any trial reached a failing verdict, error only when no trial reached a verdict, passed only when all passed; UnsafeRetry is OR'd and AgentRefunds/DuplicateRefunds take the maximum across trials; a new `verdict_trial` (1-based, omitted when trials is 1) names the trial whose FailedChecks are reported - so rates do not depend on trial order - cost if wrong: one extra report field.
Ruling: ReportSchemaVersion stays 2 - no v2 report without the fingerprint was ever released (Tasks 1 and 2 land on one branch); the Mutations comment is reworded instead, and Task 3's gate treats status error or trials 0 as unmeasured - cost if wrong: none outside this branch.
Ruling: trials with passes and errors but no failure fold to error (incomplete measurement, excluded from rates) while passes/trials still records the passes for the gate - a pass cannot be claimed for a case with an unmeasured trial - cost if wrong: such a case drops out of the summary rates.
Task 2: fix round 1/5 (2 Important + 6 minors addressed, 0 open - order-independent fold, OR/max duplicate-effect inputs, verdict_trial, comments, exact messages, --trials<1 refused, FormatText passes/trials, deterministic WallMS; commits 211c648..9b6fe59). Re-review f2-rereview1 clean.
Task 2: minor (deferred): Status error with Passes>0 must not be read as passed by consumers (gate: Task 3); FormatText test substring-only; trajectory/mutation tests run the whole standard suite twice; distributed fingerprint check only asserts len>0.
Task 2: complete (commits 329ce7c..9b6fe59, review clean)
Task 3: implementer f3-impl DONE, commit e4d86d3 (BASE 9b6fe59). Review f3-review: spec ❌, Needs fixes (3 Important). Judgement calls: 1 accepted (invalid options are exit-2 bad input), 2 accepted for expect:pass cases, 3 rejected.
Ruling: a baseline with no cases, or with duplicate case IDs, is bad input (Compare error, exit 2) - an empty baseline makes the gate unfailable - cost if wrong: an intentionally empty baseline must be replaced by a real one.
Ruling: controls are excluded from pass-rate comparison; for multi-trial controls the detection rate (Trials-Passes)/Trials is compared and a drop beyond tolerance is control_undetected - a control's Passes counts misses, so the pass-rate direction is inverted - cost if wrong: none for single-trial suites.
Ruling: UnsafeRetry and AgentRefunds comparisons are skipped only when the baseline errored; an errored candidate still reports them - bench folds them as real observations whatever the verdict - cost if wrong: an errored candidate can show two findings for one case.
Ruling: an errored baseline case yields a warning kind baseline_unmeasured; AsExpected for controls is derived from Status and Expect with bench's own rule when the stored value disagrees, so v1 files without as_expected still gate - cost if wrong: one extra warning kind.
Task 3: fix round 1/5 (3 Important + 2 minors addressed, 0 open - empty/duplicate baseline refused; control detection rate; errored-candidate unsafe retry/refunds; baseline_unmeasured; Status-derived control verdict; commits e4d86d3..417583a). Re-review f3-rereview1 clean.
Task 3: minor (deferred): detection rate counts errored trials as detections (no failed-trial count in schema); controls are also subject to new_unsafe_retry/new_duplicate_refund; Compared counts baseline_unmeasured cases; redundant error branch in asExpected; markdown/junit write failure after regressions exits 2; ADR 0030 owed by Task 4.
Task 3: complete (commits 9b6fe59..417583a, review clean)
Task 4: implementer f4-impl DONE_WITH_CONCERNS, commit a34e0e9 (BASE 417583a). Review f4-review: spec ❌, Needs fixes (2 Important). Reviewer benched healthy fixture twice against the committed baseline: 21 compared, 0 regressions, 0 warnings.
Ruling: RegressedProvider classifies by the compiled manifest's operation method only and requires the manifest; every construction site passes it - the exposed-ops fallback let a policy-hidden POST through - cost if wrong: none, matches the brief.
Ruling: the demo step-count test reads README.md and cmd/twinwright/AGENTS.md - a comment claimed a guard that did not exist - cost if wrong: a stricter test.
Task 4: fix round 1/5 (2 Important + 6 minors addressed, 0 open - manifest-bound RegressedProvider at every site, README/AGENTS step-count guard, ADR wording, enqueue+fail-on-eval refused, positive fail-on-eval, zero-warning baseline check, CI prints gate JSON; commits a34e0e9..69bb2ab). Re-review f4-rereview1 clean.
Task 4: minor (deferred): README sample output still shows N/11 markers; CHANGELOG line 200 historical "eleven steps" (check release section); README lacks gate/fail-on-eval/ADR 0030 mentions (Task 17).
Task 4: complete (commits 417583a..69bb2ab, review clean)

## Handoff (2026-10-27 01:41)
Owner instruction: from Task 5 on, no per-task reviewers; the controller plans and dispatches implementers only; the owner's reviewer does one whole-repo review after everything is complete. Overrides the SDD per-task review gate by explicit owner choice.
State: HEAD 69bb2ab pushed to origin/cloud/m2-platform. Task 5 was cancelled mid-run: uncommitted partial work in cmd/twinwright/{main_test.go,provider_timeout_test.go}, internal/agent/{anthropic,chat,openai,provider,runner}.go, internal/redact/{redact.go,redact_test.go}, new internal/agent/metadata_test.go. Next agent continues from it.
CI run 40455773165 on 69bb2ab: regression gate, build/test, security, e2e green; race pending at handoff.


## Resumed on the Windows PC (2026-10-28)
Machine: Windows 11 PC, Ryzen 9 7900X (24 threads), Go 1.27.3 linux/amd64 under WSL2 Ubuntu 24.04; make and scripts run in WSL; commits with Windows git (global gitleaks pre-commit and pre-push hooks). Docker Desktop 29.8.0, Ollama, python3 present. Fresh clone at HEAD 95b258d. Batches run one implementer at a time, no per-task review (owner's choice above).
Ruling: the checkout lives in Desktop/Projects/twinwright, not a GitHub folder | the owner named Desktop/Projects and no GitHub folder held the repo | cost if wrong: a path change only, nothing in the repository depends on it.
Ruling: the plan's "development Mac" local runs (Task 8 live model, Task 15 SQLite perf) are taken on this Windows PC under WSL2 and labelled with this machine | the Mac is not available and the plan's intent is a labelled local run | cost if wrong: figures describe a different machine than the plan named, stated plainly in the docs.
Batch 5+6: implementer DONE_WITH_CONCERNS, commits 57b6d13 (Task 5) and 02ef178 (Task 6), BASE 95b258d. Coordinator gate: git status clean; make check 0; go test -count=1 ./... 0; commit messages and diffs clean of em dashes, trailers and tool mentions.
Ruling: Retry-After missing, unparseable, zero or in the past counts as not sent and waits 1s; Interruption.retry_after_ms records the server value capped at 30000 and is omitted when none was sent; status is recorded for rate_limited and server_error only | keeps the ledger a statement of what the server said, bounded the same way the wait is | cost if wrong: one field's meaning in ADR 0031.
Ruling: replay never waits on a recorded retry | a recorded turn already carries its interruptions, so replay reads them back without reaching the wait | cost if wrong: none observed, TestRetriedRunStillReplays finishes in under 10s with Retry-After 30.
Ruling: internal/replay/retry_replay_test.go is added outside Task 6's file list | replay imports agent, so the replay check cannot live in package agent (same reason interrupt_replay_test.go lives there) | cost if wrong: one extra test file.
Batch 5+6: deferred: trace attributes omit status and retry_after_ms of provider.interrupted (trace.go picks fixed keys); README, ADR 0027 and CHANGELOG do not yet describe the new interruption kinds or --max-output-tokens (ADR 0031 and CHANGELOG in Task 8, README in Task 17); any Do error, including a bad URL scheme, is a transport interruption and is retried once after 1s.
