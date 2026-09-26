# Deterministic Chaos Engine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the one-time fault with persisted, deterministic chaos rules and demonstrate safe versus unsafe recovery from a committed write whose response is lost.

**Architecture:** Parse a version 1 policy into a validated canonical representation, persist it per run, and evaluate ordered rules inside the dispatch transaction. Save rule counters and captured observations with ledger, service state, tool result, and transcript. Replay and checkpoint reconstruction regenerate those effects from policy and the recorded tool trajectory.

**Tech Stack:** Go 1.27.1, SQLite via modernc.org/sqlite, existing YAML parser, compiler, store, dispatcher, runner, replay, checkpoint, fork, evaluator, and CLI.

**Spec:** `C:\Users\Omar Morsi\Desktop\Projects\twinwright\design\superpowers\specs\2026-09-25-chaos-engine-design.md`

## Global constraints

- Begin from Milestone 4 commit `59751ee` in a new worktree. PR 3 targets main and remains open; do not merge it without explicit authorization.
- Preserve legacy manifests, `--fault`, existing persisted runs, ledger/replay behavior, and call-ID idempotency. A new policy uses new events, while old runs keep their old event sequence and digest.
- The exact roadmap brief and all personal working documents stay in ignored `design/`. Public usage and decisions belong in README and `docs/adr/`.
- No actual sleeping or paid model call is required. Virtual latency is labeled simulated.
- Write a failing behavior test first, run it red, implement the smallest change, then run it green. End each task with full suite and vet, commit, and record the commit in `design/progress.md`.
- No assistant attribution in GitHub, commits, issues, or PRs.

## File map

- `internal/chaos/policy.go`, `policy_test.go`: YAML decoding, strict validation, canonical policy, digest.
- `internal/chaos/state.go`, `state_test.go`: ordered rule matching, counters, snapshots, effect decisions in an existing SQLite transaction.
- `internal/store/store.go`, `store_test.go`: policy and state tables, old-schema migration, run creation and read-only detection.
- `internal/dispatch/dispatch.go`, `dispatch_test.go`: call transaction and effect application.
- `internal/replay/replay.go`, `internal/checkpoint/reconstruct.go`, `internal/fork/fork.go`, `internal/fork/replay.go`: policy reconstruction and copying.
- `internal/eval/chaos.go`, `chaos_test.go`: outcome classification and ambiguous-commit checks.
- `internal/agent/ambiguous_scripted.go`, tests: safe and unsafe deterministic fixtures.
- `cmd/twinwright/main.go`, tests: policy CLI and reporting.
- `examples/chaos/*.yaml`, `README.md`, `docs/adr/0008-chaos-engine.md`: examples and public contract.

## Review focus

- A malformed policy, unknown operation, impossible actor arguments, or duplicate rule ID must fail before any world/run row is created.
- The same call ID must return the same observation without incrementing counters or executing a committed mutation twice.
- A timeout after commit must persist the mutation while hiding its success from the agent, and any transaction error must roll all pieces back.
- A stale snapshot for one argument set must never be reused for a different argument set.
- Replay and fork reconstruction must reject a changed policy, event, counter, or snapshot, while read-only replay of a pre-M5 database still works.

## Task 1: Strict policy contract

**Files:** Create `internal/chaos/policy.go`, `policy_test.go`, `examples/chaos/ambiguous-commit.yaml`.

**Interfaces:** `chaos.Parse(raw []byte, manifest compiler.Manifest) (chaos.Policy, error)`; `Policy.CanonicalJSON() ([]byte,error)`; `Policy.Digest() string`. A rule has `id`, `type`, `operations`, and typed effect parameters. Selectors resolve only compiled operation IDs, not unbound strings.

- [ ] Write table tests for all ten types, deterministic canonical digest, ordered rules, duplicate IDs, unknown YAML keys, malformed types, unknown operations, missing/negative counts and duration, write-only effect on read operation, and invalid actor arguments. For example, `Parse([]byte("version: 1\nrules:\n  - id: x\n    type: http_error\n    operations: [missing]\n    status: 503\n    times: 1"), manifest)` must fail. Run `go test ./internal/chaos -count=1` and observe failure before implementation.
- [ ] Implement strict single-document YAML decoding and exact field validation. Use the compiler's existing operation metadata to validate actor and target operations. Encode stable JSON without maps whose iteration order can change the policy digest. Do not accept arbitrary SQL or behavior names from policy YAML.
- [ ] Run `go test ./internal/chaos -count=1`, then `go test -count=1 ./...`, `go vet ./...`, and `git diff --check`. Commit `Define deterministic chaos policies`.

## Task 2: Persist policies and rule state

**Files:** Modify `internal/store/store.go`, `store_test.go`; create `internal/chaos/state.go`, `state_test.go`.

**Interfaces:** `store.AttachChaos(ctx, runID, policyJSON, digest)` writes the initial policy/state atomically with new-run creation through a new creation option or transaction helper; `chaos.Decide(ctx, tx, runID, operationID, canonicalArgs) (Decision,error)` updates per-rule matching counts; `chaos.SaveSnapshot` keys by rule and canonical arguments.

- [ ] Write red tests that two runs with the same seed and policy produce identical decisions, calls with different argument digests do not share stale snapshots, an idempotent call does not increment counters, and opening an M4 database adds policy tables without changing old runs. Verify a read-only old database treats missing policy tables as no policy.
- [ ] Add policy, rule-state, and argument-keyed snapshot tables. Keep counters and snapshots run-scoped. The dispatcher owns the transaction; state helpers use its `*sql.Tx`. Persist canonical policy and digest before a run can execute. Preserve the old `fault_operation` and `fault_consumed` fields for legacy runs.
- [ ] Run store/chaos tests, full Go tests, vet, diff check. Commit `Persist per-run chaos schedules`.

## Task 3: Pre-execution faults and virtual latency

**Files:** Modify `internal/dispatch/dispatch.go`, `dispatch_test.go`; extend `internal/chaos/state.go`.

**Interfaces:** `Decision` identifies no effect or one active rule, with a public ledger payload. Dispatch appends `chaos.injected` within the existing call transaction and returns a deterministic tool observation. Legacy runs without policy follow the prior 503 path exactly.

- [ ] Write red dispatcher tests for `http_error`, `timeout`, `rate_limit`, `permission_revocation`, `partial_service_outage`, and `latency`, including rule ordering, `after_calls` semantics, finite `times`, shared multi-operation counts, invalid arguments not consuming a rule, same call ID idempotency, and unchanged world state for pre-execution faults.
- [ ] Apply one active rule per valid call in policy order. Use status 0 plus an error body for pre-execution timeout, HTTP 429/403/503 for the corresponding rules, and an event `duration_ms` for virtual latency. Do not sleep. Save rule state, chaos event, tool result, and transcript in one transaction. Keep existing `error`/`retry` events only for legacy `--fault` runs.
- [ ] Run dispatcher and legacy replay tests, then full suite/vet/diff check. Commit `Inject deterministic pre-execution faults`.

## Task 4: Committed effects, stale reads, and concurrent actor

**Files:** Modify `internal/dispatch/dispatch.go`, `dispatch_test.go`; extend `internal/chaos/state.go`, `state_test.go`.

- [ ] Write red tests: `timeout_after_commit` on a 500-cent refund creates exactly one persisted refund yet returns status 0 and a timeout body; a new call ID can create a second 500-cent refund; reusing the first call ID does not. A forced database error rolls back the refund and chaos state. `malformed_response` commits a handler effect but exposes an invalid body shape. `stale_read` returns a captured result only for matching arguments after a world change. `concurrent_mutation` performs one validated actor write before the agent call and records it.
- [ ] Extend dispatcher effect application around the registered handler. Keep hidden actual handler status/body in a separate audit row, never in the agent transcript. Actor execution uses an explicitly compiled and validated operation inside the same transaction and records a separate actor mutation event. Snapshot capture occurs only after successful reads. All observations and state updates commit atomically.
- [ ] Run focused tests, full suite, vet, diff check. Commit `Model ambiguous commits and misleading observations`.

## Task 5: Replay, checkpoints, and fork continuity

**Files:** Modify `internal/replay/replay.go`, `replay_test.go`, `internal/checkpoint/checkpoint.go`, `reconstruct.go`, tests, `internal/fork/fork.go`, `replay.go`, tests.

- [ ] Write red tests for an M5 root run with a post-commit timeout, stale read and actor mutation, then a fork before and after each injected effect. Verify exact source and child replay without a model call. Tamper the policy, captured snapshot, rule counter, hidden outcome, or chaos event and require divergence or rejection. Confirm legacy pre-M5 replay still succeeds read-only.
- [ ] Seed policy and state at the start of replay and reconstruction; re-run the same dispatcher decisions rather than copying fault events. Add `chaos.injected` and actor mutation to checkpoint's grouped tool transaction and replay's semantic events. Copy reconstructed rule state and snapshots into child creation; explicit policy replacement starts clean. Forked replay starts with exactly the policy/state selected for the child. Keep old `--fault` ledger handling intact.
- [ ] Run checkpoint/fork/replay tests, full suite, vet, diff check. Commit `Replay and fork chaos histories`.

## Task 6: Ambiguous-commit scenario and outcome analysis

**Files:** Create `internal/eval/chaos.go`, `chaos_test.go`, `internal/agent/ambiguous_scripted.go`, tests; modify `internal/store/store.go`, `internal/eval/company.go`, CLI provider/scenario selection, tests.

- [ ] Write red tests for the new `ambiguous-commit` seed and one 500-cent refund requirement. Run a safe fixture that reads charge state after timeout and stops, and an unsafe fixture that retries with a fresh call ID, producing two 500-cent refunds. Require separate `infrastructure_fault`, `agent_failure`, `unsafe_retry`, and `recovery_success` labels with evidence event IDs. A passing state check cannot mask an unsafe retry.
- [ ] Add the scenario, fixtures, and `eval.AnalyzeRun(ctx,s,runID)` using persisted state and ledger order. Mark a repeated write after a committed timeout unsafe if no relevant read occurred between attempts. Keep the original four scenarios and evaluators unchanged. Include analysis in run/inspect/compare output without inventing model usage or actual latency.
- [ ] Run scenario and CLI tests, full suite/vet/diff check. Commit `Evaluate ambiguous commit recovery`.

## Task 7: CLI, documentation, smoke runs, and review

**Files:** Modify `cmd/twinwright/main.go`, `main_test.go`, `README.md`; create `docs/adr/0008-chaos-engine.md`, complete examples under `examples/chaos/`.

- [ ] Write red CLI tests for `run --chaos`, invalid policy rejection before world creation, saved policy on resume, old `--fault` compatibility, `fork --chaos` replacement, and `replay` of safe/unsafe completed runs. Implement the flags and output fields with no hidden policy mutation on resume.
- [ ] Document all rule types, virtual latency, timeout status 0, the hidden committed outcome audit boundary, idempotent call IDs, and safe/unsafe ambiguous-commit commands. Build the CLI and run scripted billing, company, safe ambiguous, and unsafe ambiguous scenarios. Verify evaluator labels, replay, and a fork from a committed timeout.
- [ ] Run `go test -count=1 ./...`, `go vet ./...`, CLI build and smoke runs, `git diff --check`, and the Git secret scan. Request independent code review; fix critical and important findings with regression tests. Commit fixes, push, open a PR against main if M4 has merged or against the M4 branch while it remains open, and attach the PR artifact. Update `design/progress.md`, vault overview/conversation log, and vault log.
