# Twinwright declarative assertion framework design

Status: selected design, 2026-09-25

## Intent

Milestone 7 of the saved roadmap generalizes the hard-coded per-scenario evaluators into a small, strongly validated assertion file that a user can write for any world, while keeping custom Go evaluation for cases the file cannot express. Evaluation must stay independent of execution: it reads a finished (or paused) run, never writes, and does not change replay.

## Approaches

1. A general query or expression language (SQL fragments, CEL). Rejected: the roadmap forbids a giant language, SQL fragments break world isolation and injection safety, and validation would be weak.
2. Assertions persisted with the run and evaluated during execution. Rejected: evaluation would couple to execution and replay, and assertions could not be revised after the fact.
3. **Chosen:** a version 1 YAML file of typed assertions, validated against a fixed table/column allowlist and the compiled manifest, evaluated read-only against one run's world rows and full ledger (including a fork's inherited parent prefix). Custom Go evaluators register by name.

## Assertion types

Every assertion has a unique `id` and a `type`.

- `row_count`: `table`, optional `where`, and exactly one of `equals`, `at_least`, `at_most`.
- `field_equals`: `entity: table.ID`, `field`, and exactly one of `value` or `value_from: table.ID.field`.
- `relationship`: every row's `field` in `table` names an existing `references` row in the same world.
- `event_count`, `event_exists`, `event_absent`: `event` type and optional payload `where`; `event_count` takes one count bound.
- `mutation_forbidden`: exactly one of `service` (behavior prefix present in the manifest) or `operation` (compiled operation ID). Fails when any `state.mutation` occurs inside a tool call to it, including a hidden committed timeout.
- `event_order`: `first` and `then` event matchers. Passes when every `then` event has an earlier `first` event.
- `custom`: `name` of a registered Go evaluator. Built in: `scenario_evaluation`, the existing scenario checks.

`where` on rows maps an allowlisted column to a scalar of the column's type, or `{contains: text}` for text columns. `where` on events matches top-level payload keys by JSON equality. Authorization assertions are event assertions on `authorization.allowed`/`authorization.denied`.

## Validation

Strict YAML (known fields, single document, integer version, no nulls). Table and column names come only from the allowlist, so every SQL string is built from constants and all values are bound parameters with a `world_id` predicate. Value types must match the column type. Count bounds are nonnegative. Event types must be known ledger types. Services and operations must exist in the manifest; custom names must be registered.

## Output and CLI

`twinwright evaluate <run-id> --assertions file.yaml` opens the database read-only and emits per-assertion `passed`, `detail`, and evidence event IDs, plus an overall `passed` and the assertion set digest. A failed assertion set exits nonzero after emitting the report, like replay divergence. Example files cover the duplicate-charge, ambiguous-commit, and prompt-injection scenarios.

## Limits

No arithmetic, joins beyond `value_from` and `relationship`, aggregates other than counts, or cross-run assertions. Event `where` inspects only top-level payload keys.
