# Checkpoint and Fork Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restore a verified run prefix into an isolated world, continue it under a new run ID, and compare parent and child outcomes.

**Architecture:** Discover checkpoints at committed ledger boundaries. Replay the selected prefix in memory using the existing store and dispatcher, compare it to the source, then copy only world-scoped rows and prior tool results into a new world and lineage record atomically. Child events describe the suffix; lineage and transcript expose inherited history.

**Tech Stack:** Go 1.27.1, SQLite through modernc.org/sqlite, existing compiler, store, dispatcher, runner, replay, evaluator, and CLI.

**Spec:** `C:\Users\Omar Morsi\Desktop\Projects\twinwright\design\superpowers\specs\2026-09-25-checkpoint-fork-design.md`

## Global constraints

- Branch from Milestone 3 commit `fea47fd`; PR 2 targets the Milestone 2 branch while PR 1 awaits default-branch merge approval. Do not bypass that approval review.
- Existing legacy and versioned manifests, run IDs, completed-run replay, event ordering, and billing/company evaluations stay compatible.
- The parent world and run are read-only during reconstruction; only an atomic child creation transaction writes to the destination database.
- No full SQLite file copy or checkpoint row snapshot per event. Explicit world-table row copying happens once when creating a fork.
- Forks require a paused or completed source, a valid manifest digest, a supported event boundary, and a verified ledger prefix.
- Keep the roadmap brief, this spec, and this plan under ignored `design/`; publish only architecture, usage, and tests in the repository.
- No paid model call is required for acceptance. Live provider behavior remains a separate unverified integration point without an API key.

## Review focus

- Selecting an event inside a tool transaction must fail without altering the database; test `tool.request` and `state.mutation` sequences.
- Tampering with a source event, tool result, or transcript before the checkpoint must fail before child creation.
- A child must preserve prior call ID idempotency while allowing new calls, including an initially pending tool call.
- A fork from a parent that resumed after checkpoint listing must either verify the unchanged prefix or fail on a changed prefix.
- A changed manifest, unsupported checkpoint version, or unsupported custom behavior must fail explicitly.

## Task 1: Checkpoint discovery and identity

**Files:** Create `internal/checkpoint/checkpoint.go`, `checkpoint_test.go`; modify `cmd/twinwright/main.go`, `main_test.go`.

**Interfaces:** `checkpoint.List(ctx, store, runID, manifest) ([]Checkpoint,error)` and `checkpoint.Select(..., seq int) (Checkpoint,error)`. A checkpoint has run ID, event sequence/type, format version, manifest digest, prefix digest, model turn count, tool call count, and deterministic ID.

- [ ] Write failing tests for all supported boundaries, an empty/bad run, event gaps, invalid JSON, selecting the middle of a tool transaction, a mismatched manifest, and repeatable checkpoint IDs. Run `go test ./internal/checkpoint -count=1` and observe the missing API.
- [ ] Implement a single ordered event scan. Validate sequential event IDs and complete tool event groups. Hash canonical event type and JSON payload pairs with the run ID, event sequence, manifest digest, and format version. Reject unknown or incomplete events.
- [ ] Add `checkpoints <run-id> --manifest ... --db ...` with read-only database opening. Its JSON output must include supported sequences and IDs; listing must not create a database or alter an existing one.
- [ ] Run focused tests, the full Go suite, vet, and diff check. Commit `List verified execution checkpoints`.

## Task 2: Verified prefix reconstruction

**Files:** Create `internal/checkpoint/reconstruct.go`, `reconstruct_test.go`; make only needed shared comparison helpers in `internal/replay/replay.go` reusable without changing the existing `Verify` result.

**Interfaces:** `checkpoint.Reconstruct(ctx, source, runID, selected, manifest) (*store.Store, store.Run, error)` returns an in-memory store containing the verified parent state at the boundary. Caller closes it. Expose a simple close-safe result wrapper if needed.

- [ ] Write a failing scripted billing test that pauses and reconstructs before and after `createRefund`, comparing all world-scoped rows and the transcript. Add a company incident case spanning CRM, ticket, and message mutations. Add event/result tamper cases and a completed-run case.
- [ ] Reproduce `model.request` with `StartModelCall`, `model.response` with `SaveTurn`, grouped tool events with `Dispatcher.Invoke`, and pause/completion with `SaveStatus`. Preserve the parent run ID, original call IDs, model, task, seed, scenario, manifest, and original fault-consumption history. Stop precisely at the selected committed event.
- [ ] Compare generated event types and canonical JSON payloads against the source prefix; compare tool results and transcript. Do not infer state from mutation summaries. Reject provider errors not covered by the current replay contract.
- [ ] Run focused tests plus all existing replay tests, then full suite/vet. Commit `Reconstruct verified run prefixes`.

## Task 3: Atomic isolated fork and lineage

**Files:** Modify `internal/store/store.go`, `store_test.go`; create `internal/fork/fork.go`, `fork_test.go`.

**Interfaces:** `fork.Create(ctx, sourceReadOnly, destination, selected, manifest, Options) (Result,error)` with optional provider/model/fault. Store migrations add `checkpoints` and `fork_lineage`; expose `store.Lineage(ctx, childRunID)` for inspect and replay.

- [ ] Write failing tests for parent immutability, a distinct child world/run, all 15 world-scoped service tables copied accurately, prior result idempotency, pending call continuation, new fault handling, old database migration, and failure rollback with no orphan rows.
- [ ] Add schema migrations with explicit format version. Use one destination write transaction to recheck the parent tip/prefix, allocate a world ID, insert its world row, copy allowlisted rows from the reconstructed store, insert the child run and lineage, copy only prefix tool results, and append `execution.forked`. No call to `SeedScenario` may commit part of this work before the fork transaction.
- [ ] Preserve the source fault-consumed state when carrying the same fault. Reset consumption only for an explicitly selected new fault. Validate changed provider/model and fault operation before writing. Use deterministic error messages for unsupported custom behavior state transfer.
- [ ] Run store/fork tests, full suite/vet, and `git diff --check`. Commit `Create isolated forks with lineage`.

## Task 4: CLI continuation and comparison

**Files:** Modify `cmd/twinwright/main.go`, `main_test.go`; create `internal/fork/compare.go`, `compare_test.go`; update `README.md` and add `docs/adr/0007-checkpoint-fork.md`.

- [ ] Write failing CLI tests for `fork <run-id> --at-event N`, invalid boundary, changed manifest, `--agent`/`--model`/`--fault`, and subsequent `resume` with a pending tool call. Test that `inspect` includes lineage.
- [ ] Wire the CLI to checkpoint selection, reconstruction, atomic creation, and optional execution. Report child/parent/checkpoint/world IDs and status. Keep existing `run` and `resume` defaults unchanged.
- [ ] Write a failing comparison test with a parent/child outcome difference. Implement `compare` using lineage, event suffixes, evaluator checks when available, and explicit state-table differences. Report latency and usage only if measured; otherwise mark them unavailable.
- [ ] Document CLI examples, supported boundaries, isolation, lineage, limitations, and failure behavior. Run focused and full checks. Commit `Expose fork and comparison commands`.

## Task 5: Fork-aware replay and final review

**Files:** Modify `internal/replay/replay.go`, `replay_test.go`; add CLI tests in `cmd/twinwright/main_test.go`; update public docs if the capability boundary changes.

- [ ] Write a failing test that completes a fork and verifies its suffix from the reconstructed parent checkpoint. Also test a tampered lineage record, changed parent prefix, and a child with a changed model/fault.
- [ ] Teach `replay.Verify` to detect lineage, reconstruct and verify the named parent prefix, initialize the child state and inherited transcript/results, then replay and compare only the child suffix. If this cannot be made correct in the milestone, return an explicit unsupported-fork replay error and document the limit; never use ordinary seed replay for a child.
- [ ] Run `go test -count=1 ./...`, `go vet ./...`, build the CLI, scripted CLI smoke forks for billing and company incident, a secret scan, and `git diff --check`. Request independent code review and fix concrete findings. Commit review fixes, push the M4 branch, and open a stacked PR targeting Milestone 3. Attach the PR artifact.

Record each task's commit and verification in `design/progress.md`, then update the vault project overview, conversation log, and vault log. Do not claim the later chaos, security, or observability milestones are complete.
