# ADR 0010: Declarative run assertions

Status: accepted, 2026-09-25

Scenario evaluators were Go functions, one per scenario. Adding a scenario or a new success criterion meant writing code, and a reader could not see the criteria without reading it. Twinwright now accepts a version 1 YAML file of typed assertions that is checked against one run.

A general expression language or raw SQL fragments were rejected. They are hard to validate, they invite injection and cross-world queries, and the roadmap asks for a small language. Evaluating assertions during execution, or storing them with the run, was also rejected: evaluation must stay independent of execution so that replay is unaffected and criteria can be revised after a run.

The file lists assertions with a unique ID and a type: row counts with equality or text-containment filters, a field compared to a value or another row's field, referential relationships, ledger event counts with top-level payload filters, forbidden mutations by service or operation, event ordering, and named custom Go evaluators. Authorization assertions are event assertions on `authorization.allowed` and `authorization.denied`. Parsing is strict: known fields, one document, an integer version, and no null values. Table and column names come from a fixed allowlist, so SQL is built only from constants and every value is a bound parameter with a `world_id` predicate. Value types must match column types, count bounds must be nonnegative, and services, operations, event types, and custom names must exist.

A forbidden mutation is attributed to the tool call whose request and response surround the `state.mutation` event, so a write committed behind a lost response still counts. Environment actor writes from chaos rules are not attributed to the agent. Event assertions on a fork read the parent history up to the checkpoint followed by the child's events, matching run analysis.

`twinwright evaluate` opens the database read-only, rejects a manifest that does not match the run's world, emits each result with detail and evidence event IDs, and exits nonzero when an assertion fails. The built-in `scenario_evaluation` custom evaluator runs the existing Go scenario checks, so declarative files can combine both.

The language has no arithmetic, aggregates other than counts, joins beyond `value_from` and `relationship`, nested payload paths, or cross-run assertions. Cases that need them use a custom evaluator.
