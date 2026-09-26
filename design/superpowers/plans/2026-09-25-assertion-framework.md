# Assertion Framework Implementation Plan

**Goal:** Declarative, validated scenario assertions with a custom Go escape hatch, evaluated read-only per run.

**Spec:** `C:\Users\Omar Morsi\Desktop\Projects\twinwright\design\superpowers\specs\2026-09-25-assertion-framework-design.md`

**Base:** main `4207cc6` (Milestone 6 merged). Worktree `.worktrees/milestone-7-assertions`, branch `milestone-7-assertions`.

## Task 1: Parser and validation

Create `internal/assertion/parse.go`, tests. `assertion.Parse([]byte, compiler.Manifest, Customs) (Set, error)`, `Set.Digest()`. Red tests for every type's happy path and rejection: unknown type/field/table/column, wrong value type, missing or multiple bounds, bad entity refs, unknown event type, unknown service/operation/custom, duplicate IDs, nulls, multiple documents. Commit `Define declarative scenario assertions`.

## Task 2: State and relationship evaluation

Create `internal/assertion/check.go`. `assertion.Check(ctx, *store.Store, runID, Set) (Report, error)`. Red tests against seeded and mutated worlds: counts with `where` and `contains`, `field_equals` with `value` and `value_from`, missing entity, relationship pass and dangling reference, world isolation. Commit `Evaluate state assertions`.

## Task 3: Ledger assertions

Extract `eval.Ledger` (fork prefix) from `AnalyzeRun`. Event count/exists/absent with payload `where`, `mutation_forbidden` by service and operation including hidden committed timeout, `event_order`, evidence event IDs, fork prefix included. Commit `Evaluate ledger assertions`.

## Task 4: Custom evaluators, examples, CLI, docs

`Customs`, `Builtins()` with `scenario_evaluation`. Example files under `examples/assertions/`; tests that they pass on good fixtures and fail on unsafe/overprivileged ones. `evaluate` CLI (read-only, nonzero on failure, no DB creation). README section and ADR 0010. Smoke, full gate, independent review, PR.
