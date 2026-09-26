# Twinwright Replay Verification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Verify a completed billing run by replaying recorded assistant responses in an isolated seeded world and comparing execution and final state.

**Architecture:** A replay provider feeds saved model responses into the existing runner. An in-memory Store keeps replay effects away from the source. A verifier compares semantic events, tool results, transcript, and all billing rows.

**Tech Stack:** Go 1.27.1, SQLite, existing internal packages.

**Spec:** design/superpowers/specs/2026-09-25-replay-verification-design.md

## Global Constraints

- Only completed duplicate-charge runs with valid model turns are supported.
- Preserve the source run ID for deterministic refund IDs.
- Do not call a network model or mutate the source database.
- Reuse the existing runner and dispatcher rather than duplicating billing logic.
- Compare decoded JSON and complete billing rows.

## Review Focus

- Tampered tool.response payload must fail verification even if database state is unchanged.
- Tampered billing row must fail verification even if ledger is intact.
- One-shot 503 and pause/resume history must replay successfully.
- A wrong manifest digest or incomplete run must be rejected before execution.
- A failed model response must be rejected rather than silently skipped.

---

### Task 1: Fixed-ID isolated run creation

**Files:** Modify internal/store/store.go. Test internal/store/store_test.go.

**Interfaces:** Produce Store.CreateReplayRun(ctx context.Context, original Run, worldID string) (Run, error). It inserts a new run with original.ID and metadata in the caller's isolated Store, and records execution.started.

- [ ] Write TestCreateReplayRunPreservesIDAndIsolation: seed a second in-memory Store, create a source run, call CreateReplayRun, assert the saved ID and world ID, and assert the source run is unchanged.
- [ ] Run go test ./internal/store -run TestCreateReplayRunPreservesIDAndIsolation -count=1. Expect failure because CreateReplayRun does not exist.
- [ ] Add the narrow method using one transaction, an INSERT into runs, and AppendEventTx. Return the saved Run.
- [ ] Run the targeted test and the full package tests; commit.

### Task 2: Replay verifier

**Files:** Create internal/replay/replay.go and internal/replay/replay_test.go.

**Interfaces:** Produce Verify(ctx context.Context, source *store.Store, runID string, manifest compiler.Manifest) (Report, error). Report has RunID, Verified, ModelTurns, ToolCalls, EventsCompared, and Divergence JSON fields.

- [ ] Write TestVerifyCompletedRunWith503AndPause: run the real scripted provider, pause and resume, call Verify, assert Verified, one refund, and that the source event count is unchanged.
- [ ] Run go test ./internal/replay -run TestVerifyCompletedRunWith503AndPause -count=1. Expect a missing package or missing Verify failure.
- [ ] Implement loading and validation, replay response provider, isolated store creation, runner execution, and comparison of semantic events, tool results, transcript, and complete sorted billing rows.
- [ ] Run targeted and full package tests; commit.
- [ ] Add tests that mutate one source tool.response event and one source refund reason, each of which must produce Verified=false with a useful divergence. Add tests for wrong manifest, paused run, and failed model turn.
- [ ] Watch each new test fail for its intended reason, implement only the needed validation, run package tests, and commit.

### Task 3: CLI and documentation

**Files:** Modify cmd/twinwright/main.go and cmd/twinwright/main_test.go, README.md. Add docs/adr/0004-replay-verification.md.

**Interfaces:** Command replay <run-id> [--manifest path] [--db path] emits Report JSON. Divergence exits nonzero after emitting the report.

- [ ] Write TestCLIReplayCompletedRun in cmd/twinwright/main_test.go using the existing CLI helper; assert verified JSON. Add a tampered state case and assert a nonzero error.
- [ ] Run go test ./cmd/twinwright -run TestCLIReplayCompletedRun -count=1. Expect unknown command replay.
- [ ] Wire the command to replay.Verify. Add README usage and the ADR's scope and compatibility decision.
- [ ] Run targeted test, gofmt, go vet ./..., go test -count=1 ./..., then a real CLI smoke replay; commit.

## Review fixes

- [x] Add Store.OpenReadOnly and prove a missing path remains absent, a legacy schema is not migrated, and writes are rejected.
- [x] Route only the replay CLI path through OpenReadOnly and prove it does not create a missing database.
- [x] Decode recorded model responses with UseNumber; reproduce and fix divergence from a recorded decimal-form argument.
- [x] Make negative ledger tests preserve terminal completion, exercise the intended error, and cover a different source world instance ID.
