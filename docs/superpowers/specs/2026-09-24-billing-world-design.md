# Twinwright billing world: Phase 0 design

Date: 2026-09-24

## Purpose and scope

Twinwright's long-term target is compiling descriptions of company software into executable environments for agent testing. Milestone 1 proves the smallest meaningful loop: compile a fictional billing API, create a seeded stateful world, let one agent investigate a duplicate charge and issue one justified refund, record its execution, inject one failure, resume safely, and evaluate the database state.

The name Twinwright replaces the working name WorldForge. It describes constructing executable twins without suggesting that an OpenAPI file can reconstruct an entire company.

## Boundaries and flow

One Go process contains six separately testable packages: compiler, billing behavior, dispatcher, agent runner, SQLite store, and evaluator. The CLI wires them together. OpenAPI describes only the operations, paths, parameters, and JSON schemas. A separate bindings file maps each operation ID to a known billing behavior. Compilation rejects duplicate IDs, unsupported methods/path shapes, external references, missing bindings, and bindings for nonexistent operations. It produces a normalized operation manifest. There is no generated Go source or live HTTP server in this milestone; the agent invokes operations through the dispatcher using the compiled manifest.

The dispatcher validates operation ID and arguments, applies the specified one-shot fault, and calls billing behavior inside a store transaction. Reads return current database state. `createRefund` mutates charge/refund state and returns the committed result. The runner presents the same operation manifest as provider tools, records provider input and output, dispatches calls, and persists its next cursor. The evaluator reads final state directly, independent of the agent's final message.

## Domain model

- `World`: immutable compile manifest hash, seed, and world ID; mutable SQLite billing state.
- `Customer`: ID and name.
- `Invoice`: ID, customer ID, period, amount, and subscription ID.
- `Charge`: ID, invoice ID, amount, status, created time, and refunded amount. A duplicate points to the same invoice and amount as the legitimate charge; scenario fixtures mark the expected duplicate privately, outside tool responses.
- `Refund`: ID, charge ID, amount, reason, and creation time.
- `Operation`: OpenAPI operation ID, method, path, input schema, and explicit behavior binding.
- `Run`: run ID, world ID, scenario, task, provider, status, iteration cursor, transcript, and optional fault configuration.
- `Event`: run-scoped monotonically ordered sequence and stable ID, UTC recorded timestamp, virtual world timestamp, type, operation/call correlation, and JSON payload.

The same seed and manifest produce the same initial entities, business IDs, amounts, and virtual timestamps. Each run gets a separate world instance ID, so runs cannot mutate each other's state. Given identical persisted model outputs and tool calls, dispatch and evaluation are deterministic. Live model decisions and network timing are not deterministic; the ledger stores normalized model requests and response items for eventual replay work.

## Event and transaction contract

Events are append-only rows keyed by `(run_id, sequence)`; `event_id` derives from run ID and sequence. `recorded_at` is a real UTC timestamp and `world_at` derives from the seed and logical step. The ledger records `execution.started`, `model.request`, `model.response`, `tool.request`, `tool.response`, `state.mutation`, `error`, `retry`, `execution.paused`, and `execution.completed`. Payloads are canonical JSON. Sensitive production data must never enter the fictional example.

The runner gives each tool call a stable call ID and persists intent before dispatch. For a mutating call, the refund row, charge update, mutation event, exact tool response, and run cursor/transcript advance commit in **one SQLite transaction**. A unique `(run_id, call_id)` result key makes retries with the same operation and arguments return the saved response. If the process dies before commit, nothing mutated; if it dies after commit, resume sees the saved result and does not refund again. SQLite is the single writer and source of truth for this slice. Model request intent is recorded before the provider call; a returned response or error is then recorded durably.

The once-only HTTP 503 fault is consumed in a transaction with its response and fault event. A resumed run sees the saved 503 for the same call; a subsequent new call may succeed and records a retry event. The runner exposes 503 as an ordinary tool result so the agent can choose to retry. A step limit pauses **between** completed provider/tool steps and writes the resume cursor. Resuming sends the persisted conversation to the provider; it never repeats a committed tool call. This is checkpointing, not full counterfactual replay.

## Agent contract

The provider boundary accepts a task, ordered transcript, and normalized tool definitions, and returns an assistant message with zero or more tool calls. One OpenAI Responses adapter is implemented for live use. An in-process scripted provider is used in tests to prove orchestration without network or model variability. Provider requests and responses are ledger events. The runner does not trust a success claim from the provider; evaluation reads billing tables.

## Evaluation

The scenario fixture identifies the expected duplicate charge and legitimate charge. Passing requires exactly one refund, on the duplicate, for the duplicate's full amount, and no refund on the legitimate charge. The evaluator returns machine-readable checks and overall pass/fail. No LLM judge is used.

## Repository shape

```text
cmd/twinwright/             CLI
internal/compiler/          OpenAPI subset and bindings
internal/billing/           scenario fixtures and state transitions
internal/dispatch/          argument validation and tool routing
internal/agent/             runner and provider interface
internal/store/             SQLite schema, transactions, ledger, checkpoints
internal/eval/              deterministic scenario checks
examples/billing/           OpenAPI and bindings
docs/adr/                   technology and contract decisions
docs/superpowers/           spec and executable plan
```

## Technology decisions

Go keeps the runtime, CLI, and transaction boundary in one deployable binary; its interfaces allow other providers later. SQLite provides portable, transactional state and event storage without a service dependency. Python, PostgreSQL, Docker, and OpenTelemetry remain plausible later, but add operational burden before this vertical slice proves its core behavior. Use a small YAML parser for the constrained OpenAPI subset and a pure-Go SQLite driver. Reject unsupported spec features instead of silently pretending they work.

## Risks and limits

OpenAPI cannot define business semantics, so bindings are mandatory. Exactly-once external effects are impossible without cooperation from the external service; this milestone can guarantee once-only **local simulated** effects via transaction and idempotency key. Real model output may be invalid or unhelpful, so the runner caps steps and records errors. SQLite serializes writes; distributed execution will need a different coordination design. Complete replay needs model response capture, compatibility policy, and a replay engine, all deferred.

## Deliberately deferred

No generic enterprise compiler, database-schema import, MCP import, RBAC/ABAC, multi-agent orchestration, distributed workers, production service connections, container isolation, UI, shadow mode, model comparison, benchmark suite, or human approval gates. No generic workflow language or plugin framework is introduced for five billing operations.
