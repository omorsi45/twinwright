# Twinwright development progress

Current checkout: `C:\\Users\\Omar Morsi\\Desktop\\Projects\\twinwright` on `main` at `40b7185` (PR 13 merged; README capability bullet follow-up).
Roadmap Milestones 1 through 14 foundation are on main. No open milestone PRs.
Nothing further required for the current roadmap slice; Postgres/workers stay deferred until an explicit later design.

## Milestone 2 tasks

- Task 1: scenario-aware store and seeded company state. Complete, commit `83bc49e`; store tests passed.
- Task 2: explicit company operation compilation. Complete, commit `bf79f9c`; billing manifest digest unchanged, compiler tests passed.
- Task 3: CRM stateful handler. Complete in integration commit `93711ec`.
- Task 4: ticketing stateful handler. Complete in integration commit `93711ec`.
- Task 5: messaging stateful handler. Complete in integration commit `93711ec`.
- Task 6: scenarios and evaluation. Complete, commit `086bcfe`; tests were red on unsupported scenario, then three CLI scenario tests passed.
- Task 7: company replay. Complete, commit `feda03a`; replay tests cover all company tables and preserve old billing replay.
- Task 8: public documentation and review. Complete, commits `a0e733e` and `1093429`.

Dispatcher routes all four service prefixes. The typed-nil billing mutation regression was corrected. Full `go test -count=1 ./...`, `go vet ./...`, `git diff --check`, and CLI build passed after code review fixes. Four built CLI smoke runs (three company variants plus legacy billing) completed with passing DB evaluations and replay; incident smoke injected one 503 on message post. Independent reviewer found three important contract gaps and one minor input-validation gap, all fixed in `1093429`. Gitleaks pre-commit hook found no leaks. `go test -race` could not run on this Windows host: Go has CGO disabled and no C compiler is installed. Milestone 2 branch is clean at `1093429`, pending GitHub integration. Later roadmap milestones remain unimplemented.

Milestone 2 was pushed to GitHub as PR https://github.com/omorsi45/twinwright/pull/1. Keep the worktree for PR feedback. Main remains at `324facf` until the PR lands.

## Next milestone

Milestone 3 design and plan are saved at `design/superpowers/specs/2026-09-25-world-definition-design.md` and `design/superpowers/plans/2026-09-25-world-definition.md`. Read both plus the roadmap's Milestone 3 section (around line 478) before coding. The plan starts from Milestone 2 commit `1093429`. Do not treat PR 1 or Milestone 2 as completion of the full roadmap.

Automatic approval review rejected merging PR 1 into default `main`: the merge changes GitHub's default branch and was not explicitly approved. The PR remains open. Omar was asked directly for merge approval; do not attempt an indirect merge or bypass. M3 can proceed on a local branch from `1093429` while waiting.

Milestone 3 branch/worktree: `C:\Users\Omar Morsi\Desktop\Projects\twinwright\.worktrees\milestone-3-world-definition`, created from `1093429`. Task 1 registry commit `87e6f02` passed independent review. Task 2 parser commit `9b69934` had a reviewer-found fractional-version bug fixed in `9c50cf5`. Task 3 generic compiler commit `b8631d6` had reviewer-found ambiguity, unsupported nested feature, and nil/empty metadata issues fixed in `355dd28`. Task 4 CLI and example commit `0e43f70`; public docs commit `b92a437`. Whole-branch review found hard-coded operation names, multi-document YAML acceptance, and normalized parent paths. Commit `fea47fd` fixes those and the review follow-up's retry remapping edge case. The full `go test -count=1 ./...`, `go vet ./...`, `git diff --check`, CLI build, three versioned-world smoke runs with evaluation/replay and incident 503 recovery, and push secret scan passed. Tests now cover renamed operations end to end, changed-binding digests, and changed-manifest resume/replay rejection. The branch was pushed and stacked PR https://github.com/omorsi45/twinwright/pull/2 targets `milestone-2-company-world`. PR 1 remains open because automatic approval review rejected merging into default `main` without explicit approval. Do not bypass this. No live model run has been verified without an API key. Milestones 4 through 14 remain open.

After each task, record commit, tests, and any ruling here. Do not infer completion from the roadmap text.

## Milestone 4 preparation

The checkpoint and fork spec is `design/superpowers/specs/2026-09-25-checkpoint-fork-design.md`; the task plan is `design/superpowers/plans/2026-09-25-checkpoint-fork.md`. Both are private and Git-ignored in the main checkout. They were derived from the roadmap's Milestone 4 section (around line 539) and the committed M3 storage/replay contracts. The recommended approach reconstructs a verified event prefix in memory and copies only world-scoped rows once when creating a fork. Worktree `C:\Users\Omar Morsi\Desktop\Projects\twinwright\.worktrees\milestone-4-checkpoint-fork` branches from `fea47fd`.

- Task 1: checkpoint discovery and read-only CLI listing. Complete at `ad4ef77`. Failing package and CLI tests were observed before implementation. Full `go test -count=1 ./...`, `go vet ./...`, `git diff --check`, and gitleaks commit scan passed. The command supports paused/completed root runs and lists model-response, tool-response, pause, and completion boundaries. No public Milestone 4 PR yet.
- Task 2: prefix reconstruction. Complete at `c2ba903`. Tests reproduce a tool boundary, a billing refund before completion, and a completed company incident across CRM, ticketing, and messaging; tampered prefix payloads and saved tool results fail. Full Go tests, vet, diff check, and gitleaks commit scan passed.
- Task 3: atomic child creation and lineage. Complete at `ac25947`. Tests cover a distinct child world/run, unchanged parent, inherited call ID idempotency, pending tool call continuation, new fault isolation, all 15 company world tables, rollback on lineage failure, and migration of an older run table. Full Go tests, vet, diff check, and gitleaks commit scan passed. The source prefix and ledger tip are rechecked inside the child creation transaction.
- Task 4: fork and compare CLI, lineage inspection, README, and ADR 0007. Complete at `06208de`. Focused tests were red before implementation, then the full `go test -count=1 ./...`, `go vet ./...`, `git diff --check`, and gitleaks commit scan passed. Immediate OpenAI continuation without an API key fails before child creation.
- Task 5: fork-aware replay and final review complete at `59751ee`. Tests cover changed child model/fault, completion-point forks without new model turns, inherited prefix-only tool results, tampered lineage and parent prefix, and legacy read-only replay. Independent review found a model-request verification gap, a legacy schema replay regression, and failed-fork preflight writes; all three were fixed with red-green regression tests. Full `go test -count=1 ./...`, `go vet ./...`, build, `git diff --check`, gitleaks commit scan, and built-CLI billing/company forks from tool-response checkpoints with successful replay and comparison passed. The branch is clean. Do not claim later milestones complete.

Milestone 4 branch `milestone-4-checkpoint-fork` was pushed at `59751ee`, and PR https://github.com/omorsi45/twinwright/pull/3 targets `main`. GitHub PRs 1 and 2 were merged by an external action on 2026-09-25 at 07:31 UTC, so their feature branches no longer exist remotely. The local main checkout was fast-forwarded to the resulting `5a9038b`. PR 3 is open; do not merge it without explicit authorization. Milestones 5 through 14 remain to be implemented. Next: read the roadmap Milestone 5 chaos section and design/plan its implementation from commit `59751ee` in a new worktree.

## Milestone 5 preparation

The roadmap's Milestone 5 chaos and ambiguous-commit section was read. Private design: `design/superpowers/specs/2026-09-25-chaos-engine-design.md`. Private plan: `design/superpowers/plans/2026-09-25-chaos-engine.md`. Both are indexed in `design/README.md` and ignored by Git. The plan selects a persisted per-run policy and transactional rule-state engine, preserving old `--fault` runs and extending replay/checkpoint/fork. Task 1 is strict policy parsing. Base commit is M4 `59751ee`; PR 3 remains open.

Milestone 5 worktree: `C:\Users\Omar Morsi\Desktop\Projects\twinwright\.worktrees\milestone-5-chaos`, branch `milestone-5-chaos`, based on `59751ee`.
- Task 1: strict version 1 policy parser and example. Complete at `590f5cc`. Tests were red with missing parser, then validated all ten types, invalid inputs, deterministic digest, and rule order. Full `go test -count=1 ./...`, `go vet ./...`, `git diff --check`, and gitleaks commit scan passed. The branch is clean. Task 2 persistence is next.
- Task 2: transactional per-run policy, rule counters, argument-scoped snapshots, and legacy read-only absence detection. Complete at `1c133fd`. Focused tests were red with missing APIs, then green. Full Go tests, vet, diff check, and gitleaks commit scan passed. Dispatcher integration is next; a stored policy currently has no effect on tool calls.
- Task 3: pre-execution `http_error`, `timeout`, `rate_limit`, `permission_revocation`, `partial_service_outage`, and virtual `latency` effects. Complete at `fd4ea50`. Dispatcher tests were red before effect wiring, then green; they cover status sequence, shared counters, idempotent call IDs, invalid arguments, and unchanged refund state. Full Go tests, vet, diff check, and gitleaks commit scan passed. Stored policies now affect these six types. Post-commit, stale-read, malformed-response, and actor effects remain for Task 4; replay/checkpoint/fork continuity remains for Task 5.
- Task 4: `timeout_after_commit`, `malformed_response`, argument-scoped `stale_read`, and transactional `concurrent_mutation`. Complete at `a90052a`. Red-green tests prove a committed 500-cent refund hidden by status 0, a second refund with a new call ID, rollback when audit insert fails, hidden real response storage, stale observation without cross-argument leakage, and actor mutation before agent read. Full Go tests, vet, diff check, and gitleaks commit scan passed. A focused independent review of Tasks 1-4 is running. Task 5 must extend replay/checkpoint/fork before chaos runs can be claimed replayable.
- Task 5: replay, checkpoint, and fork continuity committed at `3268325`. Replay regenerates chaos events and compares policy, counters, snapshots, and hidden outcomes. Checkpoint reconstruction replays chaos tool groups; forks copy only prefix chaos state. Tests cover root replay, child forks before and after injected effects, and policy/event/counter tampering. The independent review's four findings were addressed: behavior-aware read/write validation, impossible actor argument checks, post-effect service rejections, and general `after_calls`. Full Go tests, vet, diff check, and gitleaks commit scan passed. Task 6 ambiguous-commit scenario and evaluation is next.
- Task 6: ambiguous-commit scenario, safe and unsafe scripted fixtures, 500-cent state evaluator, run analysis with infrastructure fault, agent failure, unsafe retry, and recovery success evidence, and replay support committed at `19a6175`. Tests were red on missing scenario APIs and unsupported replay, then green. Safe fixture reads the charge after hidden committed refund; unsafe fixture repeats with a new call ID and produces two refunds. Full Go tests, vet, diff check, and gitleaks commit scan passed. Task 7 CLI, documentation, smoke runs, independent review, and PR remain.
- Task 7: CLI `run --chaos`, scripted safe/unsafe recovery, `fork --chaos` replacement with lineage, analysis in run/inspect/compare, public README, ADR 0008, and example policies committed at `4e8e05f`. Preflight rejects invalid policies before database creation. CLI tests cover safe/unsafe outcomes, replay, fork replacement, zero `after_calls`, simulated latency, and inherited unsafe retry. Built CLI smoke runs passed legacy billing, company incident, and safe/unsafe ambiguous runs with four successful replays; unsafe evaluation failed as intended and `unsafe_retry` was true. Independent branch review found six issues, fixed with red-green tests: actual pre-M5 read-only replay, stale snapshot cross-operation collision, zero call-gate replacement replay, inherited ambiguity analysis, infrastructure fault classification, and repeated ambiguity reset. Full Go suite, vet, diff check, and gitleaks commit scan passed after fixes. Next: verify GitHub PR base, push branch, open and attach the PR, update vault project pages and log. Do not merge without explicit authorization.
- Milestone 5 branch `milestone-5-chaos` pushed at `4e8e05f`; PR https://github.com/omorsi45/twinwright/pull/4 opened and attached against main. PR 3 had been merged externally before PR 4. The vault overview, conversation log, index, and vault log were updated. Do not merge PR 4 without explicit authorization. Next roadmap work is Milestone 6; read the exact saved brief's Milestone 6 section, make a private design and plan, and use a new worktree from `4e8e05f` while PR 4 remains open. No live model call has been verified without an API key.

## Milestone 6 preparation

Read the roadmap's identity, authorization, and adversarial security requirements. Saved a selected design at `design/superpowers/specs/2026-09-25-principal-authorization-design.md` and a task plan at `design/superpowers/plans/2026-09-25-principal-authorization.md`, indexed in `design/README.md`. Chosen boundaries: strict persisted principal policy, operation filtering plus transactional dispatcher enforcement, relationship-scoped resource checks, deterministic temporary grants/revocation, replay/fork continuity, and a ticket prompt-injection fixture. The user has repeatedly requested recommended choices without check-in questions. Start an isolated worktree from M5 `4e8e05f`, run baseline tests, and execute Task 1 with red-green tests. PR 4 remains open; no merge authorization was given.

Milestone 6 worktree: `C:\Users\Omar Morsi\Desktop\Projects\twinwright\.worktrees\milestone-6-authorization`, branch `milestone-6-authorization`, based on `4e8e05f`. Baseline tests passed. The Codex session hit its usage limit before writing any M6 file; work resumed in Cursor on 2026-09-25. PR 4 was merged on GitHub in the meantime, so the M6 PR should target `main`. Local main is still at `5a9038b` and needs a fast-forward pull.
- Task 1: strict version 1 principal policy parser, built-in behavior-to-permission allowlist for all 18 behaviors, canonical JSON with sorted sets, digest, and `examples/security/support-policy.yaml`. Complete at `6b58495`. Tests were red on undefined `Parse`/`Permission`, then green; each of 22 rejection cases was confirmed to fail on its intended check. A permission must be in the registry and used by some operation in the manifest. Omitted resource scope is `nil`; an explicit empty list is kept as `[]`. Full `go test -count=1 ./...`, `go vet ./...`, `git diff --check`, and gitleaks commit scan passed. Task 2 (identity and policy persistence in `internal/store`) is next.
- Task 2: `principal_id` run column (migrated rows `legacy-local`, new unrestricted runs `local-unrestricted`), `run_auth` and `auth_state` tables, `CreateRunConfigured`/`RunOptions` (principal read from the policy), `AuthPolicy`, read-only fallbacks. Fork inserts now copy the principal (red without the fix: child got `legacy-local`). Complete at `47237f0`.
- Task 3: `authz.Decide` and `authz.Exposed`, relationship customer scope, channel/project scope, broad-search denial, refund cap, stable reason codes, call counter. Complete at `60850d2`.
- Task 4: dispatcher authorizes after validation and before chaos/fault/handler; `authorization.allowed`/`authorization.denied` events; audit-failure rollback test. Complete at `47fa7ba`.
- Task 5: runner and checkpoint reconstruction use `Exposed`; hidden direct calls get 403. Complete at `5ca4c91`.
- Task 6: replay attaches policy, treats authorization events as semantic, compares `run_auth`/`auth_state`, checks principal matches policy; fork inherit (`authz.CopyRun`) or `fork.Options.AuthPolicyRaw` replacement with `fork_lineage.auth_replaced`. Mutation check confirmed the inherited-fork test fails without the counter copy. Complete at `7ec1917`.
- Task 7: `prompt-injection-ticket` seed (ISS-104 plus injection comment), `agent.SecurityScriptedProvider`, `eval.AnalyzeSecurity` (attempted/blocked/successful with event IDs), task evaluation, overprivileged example. Complete at `5690c31`.
- Task 8: CLI `run --auth`, `fork --auth`, `security` output in run/resume/fork/inspect, README section, ADR 0009. Six built-CLI smoke runs with replay passed. Complete at `1b68d56`.
- Independent review: no critical findings; all seven contracts held. Fixed at `5667515` with regression tests: null/blank policy values rejected, handler-invalid arguments return 400 without consuming a call on secured runs (`internal/authz/args.go` mirrors handler checks), `legacy-local` rejected on current databases. Documented: customer scope does not restrict messaging, security report does not classify leaks via search/channel-read results, revocation wins over temporary grants.
- Merged origin/main (Omar's README rewrite) at `ff83fea`, took his README and added the authorization section, guarantee, events, layout, ADR entry, and safety note. Full suite passed after merge. Pushed; PR https://github.com/omorsi45/twinwright/pull/5 against main is MERGEABLE/CLEAN. Do not merge without explicit authorization. Local main fast-forwarded to `009c5ad`. Milestones 7 through 14 remain; read the roadmap's Milestone 7 section next.
- PR 5 was merged by Omar at 20:32 UTC (`4207cc6`).

## Milestone 7 assertion framework

Spec `design/superpowers/specs/2026-09-25-assertion-framework-design.md`, plan `design/superpowers/plans/2026-09-25-assertion-framework.md`. Worktree `.worktrees/milestone-7-assertions`, branch `milestone-7-assertions` from `4207cc6`.
- Task 1 parser and allowlist validation, `453a1ac`. Each of 32 rejection cases confirmed on its intended rule.
- Task 2 state assertions (`row_count` with equality/contains, `field_equals` value/value_from, `relationship`), `aa24085`.
- Task 3 ledger assertions (event count/exists/absent, `mutation_forbidden` via enclosing tool call, `event_order`, custom) and `eval.Ledger` extracted from `AnalyzeRun`, `cb9ed46`.
- Task 4 `scenario_evaluation` builtin, example files for duplicate-charge, ambiguous-commit, prompt-injection-ticket, `evaluate` CLI, README section, ADR 0010, `f68c6a8`. Built-CLI smoke on world and billing manifests matched expectations.
- Independent review: no critical. Fixed at `516aa4e`: `Set` fields sealed (hand-built sets reached SQL), empty strings rejected, declared type in results, digest covers manifest. Deferred and documented: read-only opens in WAL mode create `-wal`/`-shm` and fail in non-writable directories (shared with replay; needs a store-level decision).
- Pushed; PR https://github.com/omorsi45/twinwright/pull/6 against main. Do not merge without explicit authorization. Next: Milestone 8 counterfactual debugger (roadmap line ~860).

- PR 6 was merged by Omar at 20:54 UTC (`fe32d01`).

## Milestone 8 counterfactual debugger

Spec `design/superpowers/specs/2026-09-25-counterfactual-debugger-design.md`, plan `design/superpowers/plans/2026-09-25-counterfactual-debugger.md`. Worktree `.worktrees/milestone-8-counterfactual`, branch `milestone-8-counterfactual` from `fe32d01`. Local main fast-forwarded to `fe32d01`. Merged worktrees for milestones 2 to 7 were removed after confirming their branches are merged and clean. Baseline tests passed.
- Task 1 observation override on forks, `10741d0`. `fork.Options.Observation` replaces the checkpoint call's observation in the child transcript and saved result, records `fork_observations` and an `observation.overridden` event after `execution.forked` in the same transaction. `ReconstructForReplay` applies the same change; replay treats the event as semantic. Tests were red on the missing API, then the replay test was red on the unknown event type. Tampering with the row body, deleting the row, editing the event, or editing the child saved result all diverge. Mutation check: skipping the saved-result override in replay reconstruction made the positive replay test fail. One early run accepted a tampered event because it compiled before the semantic-list edit landed (tests were run in the same batch as the edit); 80 repeated runs after the edit all passed. Full gate and gitleaks passed.
- Task 2 ambiguous fixture completes after a delivered 201 refund (safe and unsafe), `d71e843`. Red on "expected lost refund response", then green; a 403 first result still errors. Only previously failing paths change.
- Task 3 intervention parser, `bec7c7e`. Strict version 1 YAML, five kinds (`chaos_policy`, `auth_policy`, `fault`, `model`, `tool_response`), per-kind allowed fields, inline policies validated by `chaos.Parse`/`authz.Parse` and kept as raw YAML for the fork, canonical body, digest bound to the manifest. All 26 rejection cases were logged once and confirmed on their intended rule; one expectation substring was corrected.
- Task 4 analysis engine, `95dee84`. `Analyze` requires a completed, failing root run; plans and validates every fork (targets, fork options, providers) before creating any; forks per candidate and trial, runs the child, judges it (assertions or scenario evaluation), ranks by changed/forks. Ambiguous unsafe run: latency-only replacement at the lost-response dispatch corrected 2/2, observation override 2/2, safe model after the lost response 2/2, extra latency 0/2, replacement at the retry 0/2, safe model after the retry errored 2/2. Prompt injection: support policy at the ticket read or the foreign lookup corrected 1/1, later calls 0/1. Parent ledger and state unchanged; every completed fork replays; report identical across runs apart from run IDs. Mutation checks: dropping the observation option and dropping the auth replacement each turned a test red.
- Task 5 CLI `counterfactual`, examples `examples/counterfactual/{ambiguous-commit,prompt-injection-ticket}.yaml`, README section, ADR 0011, `25deaaa`. `Analyze` split into `Prepare` (read-only validation) and `Run` so the database is opened for writing only after validation, like `fork`. CLI test red on unknown command, then green. Built-CLI smoke: billing unsafe run with 3 trials matched the README summaries; forks replay, `compare` and `evaluate` accept them; the override fork shows `observation.overridden`; security run with support policy corrected at the ticket read and foreign lookup; usage errors actionable.
- Independent review (read-only subagent): core contracts held (isolation, atomicity, replay and tamper detection, idempotency, unchanged runs without overrides, old databases). Important: a paused and resumed parent lost its model-decision candidates because `execution.paused` sits between the tool response and the next model request. Minor: no explicit root-run error; parser truncated fractional status, lost big-number precision in bodies, accepted non-string keys, and rejected valid null/empty bodies; interventions that changed nothing (same model behavior, same fault) or two things (chaos policy on a legacy-fault run) were accepted; `observation.overridden` missing from the assertion allowlist. All fixed at `09af1b0` with red-green tests; the smoke was re-run including a paused and resumed parent. Not changed: `--steps` has no upper bound (same as `run` and `fork`); missing-database and unknown-run errors (`out of memory (14)`, `sql: no rows`) are pre-existing across `replay`/`fork`/`evaluate` and worth a separate small change.
- Pushed; PR https://github.com/omorsi45/twinwright/pull/7 against main (no conflicts with `origin/main` at `fe32d01`; gitleaks on `origin/main..HEAD` clean). Do not merge without explicit authorization. Next: Milestone 9 observability (roadmap line ~922), stacked on `milestone-8-counterfactual` while PR 7 is open.

## Milestone 9 observability

Spec `design/superpowers/specs/2026-09-25-observability-design.md`, plan `design/superpowers/plans/2026-09-25-observability.md`. Worktree `.worktrees/milestone-9-observability` from `09af1b0` while PR 7 was open. PR 7 merged during this work (`36c32ef`); the branch contains only Milestone 9 commits relative to main.
- Task 1 redaction and token usage, `373b837`. The first commit attempt was blocked by gitleaks on a high-entropy fixture key in the redaction test; the fixture is now assembled at runtime and the gate script exits if the commit itself fails. A 401 body that echoes the key is stored redacted. Usage is omitted when the response has none.
- Task 2 ledger trace projection, `296f690`.
- Task 3 CLI `trace` (json, text, otlp) and inspect summary first, `22cf79f`.
- Follow-up `44dadc5`: summed durations are rounded to milliseconds, and the text tree starts at the first model call instead of printing completion above the calls.
- Task 4 README section and ADR 0012, `8cc0519`.
- Independent review: important gap was read-time redaction of a configured secret that is not an `sk-` key. Fixed at `6399a19` by passing `OPENAI_API_KEY` into `trace.Build`. Same commit labels chaos `permission_revocation` (mutation check: without the rule the span is `client_error`) and keeps scalar fields of an actor mutation. WAL sidecars on read-only open remain the known store limitation; README now says trace and inspect share it. Not changed: no collector delivery, no live model run.
- Pushed; PR https://github.com/omorsi45/twinwright/pull/8 against main. Omar merged PR 8 (44d25e1).

## Milestone 10 providers

Spec design/superpowers/specs/2026-09-25-providers-design.md, plan design/superpowers/plans/2026-09-25-providers.md. Recovered uncommitted WIP from the abandoned worktree onto 44d25e1. Removed merged M8/M9 worktrees after confirming clean.
- Task 1 allowlist KnownProvider and fork validation, 9e07f70.
- Task 2 OpenAI-compatible chat completions adapter, b0c6dbb.
- Task 3 Anthropic Messages adapter, feab956.
- Task 4 CLI --base-url, preflight, README, ADR 0013, 434a446 / 7c00473.
- Review fix e9569c3: coalesce consecutive Anthropic tool results, is_error on failed statuses, disable_parallel_tool_use, chat tool-assistant null content, counterfactual --base-url.
- Built CLI smoke: scripted duplicate-charge completed; anthropic and openai-compatible preflight create no database. Full gate and gitleaks passed. No live provider call.
- Pushed; PR https://github.com/omorsi45/twinwright/pull/9 against main. Do not merge without authorization. Next: Milestone 11 Twinwright Bench.

## Milestone 11 Twinwright Bench

Spec design/superpowers/specs/2026-09-25-bench-design.md, plan design/superpowers/plans/2026-09-25-bench.md. Worktree .worktrees/milestone-11-bench stacked on Milestone 10 (PR 9).
- Task 1 suite parser, 1ed6d9.
- Task 2 runner and report aggregates, f8341e.
- Task 3 standard suite (16 curated cases) and one-shot outage policy, 80caf3.
- Task 4 CLI ench, report compare, README, ADR 0014, 5d1c109.
- Review fixes e9c2c85 / 4379fa: chaos ops the scripted fixture actually calls, http_error outage, fault validation, seed pointer, per-case model, Duplicate Effects labeled as incidence, pinned standard suite outcomes.
- Built CLI smoke of the full standard suite passed the pinned expectations. Full gate and gitleaks passed. No live provider bench.
- Pushed; PR https://github.com/omorsi45/twinwright/pull/10 against main (includes M10 commits until PR 9 merges). Do not merge without authorization. Next: Milestone 12 shadow mode after PRs land, or design it stacked.

## Milestone 12 shadow mode

Spec design/superpowers/specs/2026-09-25-shadow-design.md, plan design/superpowers/plans/2026-09-25-shadow.md.
- Observe-only config, JSONL source, local simulate, compare, CLI, README, ADR 0015 at dc650d4 / b8d9d4.
- Write mode and allow_writes rejected. Experimental label required. No production connectors.
- Full gate and gitleaks passed. Do not merge without authorization. Next: Milestone 13 optional Docker worlds only if isolation needs it; Milestone 14 distributed runtime only after single-node semantics stay solid.

## Post-merge sync (2026-09-25)

Omar merged PRs 9, 10, and 11. Local main fast-forwarded to 2c46166. Removed milestone-10/11/12 worktrees.

## Milestone 13 optional containers + Milestone 14 deferred ADR

- Container package and tests 7c0cd0; CLI, examples, ADR 0016 and ADR 0017 7d2bb6f.
- Docker daemon was not running; live docker start not verified. Local runtime CLI verified.
- Roadmap implementation complete through optional containers; distributed runtime explicitly deferred.

## Milestone 13 merged

Omar merged PR 12 (69e7260). Optional containers and ADR 0017 are on main. Worktree removed. Next coded work only if Omar asks for distributed runtime or a store-level WAL fix.

## Milestone 14 distributed foundation (leases)

Spec `design/superpowers/specs/2026-09-25-distributed-foundation-design.md`, plan `design/superpowers/plans/2026-09-25-distributed-foundation.md`.
- Task 1 lease package with fencing tokens `fdbe515`.
- Task 2 CLI `twinwright lease`, README, ADR 0018, ADR 0017 superseded-in-part `aaac55b`.
- Recommended slice only: leases + fencing, not a Postgres world-store rewrite.
- Full gate and gitleaks passed. Built CLI smoke for acquire/status/renew/release. Omar merged PR 13 (54cf923).

## Post-merge sync (PR 13)

Omar merged Milestone 14 (54cf923). Local main fast-forwarded. Removed milestone-14-distributed worktree. Full `go test -count=1 ./...` green on main. README lease section, layout, and ADR list confirmed; added the missing capability bullet under What Twinwright does. Roadmap complete for now; no further milestone work recommended until Omar asks.

