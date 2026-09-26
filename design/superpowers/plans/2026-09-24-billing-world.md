# Twinwright Milestone 1 implementation plan

Date: 2026-09-24. Follow the [design](../specs/2026-09-24-billing-world-design.md). Use test-driven development for behavior, focused commits, and `go test ./...` plus CLI smoke tests before completion.

## 1. Repository and build

Initialize a Go module, `.gitignore`, README, fictional example files, and an isolated feature worktree. Establish a passing empty baseline with `go test ./...`. Keep all implementation within the proposed package boundaries.

## 2. Compiler

Write failing tests for a valid five-operation OpenAPI/bindings pair and for unbound, duplicate, unsupported, and external-reference input. Implement a constrained parser and normalized manifest. The build CLI validates and writes a manifest with a stable content digest. Verify same input yields same digest.

## 3. Store and seeded world

Write failing tests for same-seed equality, different-seed variation, relationship integrity, ledger ordering, and transaction rollback. Implement SQLite schema and deterministic fictional billing fixtures. The scenario includes one legitimate and one duplicate charge. Verify initial state snapshots and event IDs.

## 4. Dispatcher and billing behavior

Write failing tests for customer/invoice/charge reads, one successful refund, subsequent charge read reflecting refund, invalid refund, and repeated call ID returning the original response with one mutation. Implement dispatch with explicit bindings and transactional handlers. Verify SQL state and event ledger, not just response text.

## 5. Failure and checkpoint

Write failing tests for once-only 503, later retry success, pause after a bounded step, and resume without repeating a refund. Implement persistent fault state, run cursor, and saved tool responses in the same transaction as effects. Verify close/reopen SQLite between pause and resume.

## 6. Agent runner and provider

Write failing tests with a scripted provider that investigates, sees a 503, retries, refunds, and stops. Implement provider interface and runner. Add a single OpenAI Responses adapter, with HTTP-level tests for function-call parsing and network/error handling. Keep provider code out of billing, store, and evaluator.

## 7. Evaluation and CLI

Write failing tests for passing and failing world states. Implement deterministic evaluator and JSON inspection. Wire `build`, `run`, `resume`, and `inspect`. Run an end-to-end scripted scenario in tests and a CLI smoke test. Document setup, example commands, live-provider requirements, and limits.

## 8. Review and finish

Run formatting, vet, full tests, and smoke tests with fresh output. Review manifest rejection, transaction atomicity, checkpoint semantics, and docs against the original brief. Address concrete defects. Keep the repository local until publishing is explicitly requested or clearly authorized.
