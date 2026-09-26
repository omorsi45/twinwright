# Milestone 9: observability design

Roadmap section: `briefs/2026-09-25-platform-roadmap.md`, "MILESTONE 9: OBSERVABILITY" (line 922). Also SECURITY (redaction before expanding integrations).

## Intent

Make a run understandable without reading raw events: a compact summary in `inspect`, a `trace` command that renders the run as a span tree (run, model invocations, tool calls, with authorization checks, faults, retries, state mutations, checkpoints, fork origin, and evaluation), and an OpenTelemetry-shaped export. Never leak secrets.

## Current state

- The ledger already records every needed fact. `recorded_at` is wall-clock time at commit (real, not simulated); `world_at` is virtual time. Chaos latency is simulated and already summed by `eval.AnalyzeRun`.
- `inspect` emits a map (keys sorted alphabetically), so raw `events` appear before `run`; there is no summary.
- The OpenAI adapter puts the provider's raw error body in the error message and in `Message.RawBody`, both of which are written to the ledger by `FailModelTurn`. Token usage in the Responses API reply is ignored.

## Approaches considered

1. Instrument the runtime with the OpenTelemetry Go SDK and export live spans. Rejected for now: it adds a dependency tree, needs a collector to be useful, and would duplicate what the ledger already records durably. Live spans would also be lost on crash while the ledger survives.
2. Derive traces from the ledger at read time (chosen). The ledger is the source of truth; the trace is a deterministic projection. Span and trace IDs are hashes of run ID and event sequence, so the same run always yields the same trace. An OTLP/JSON file export gives OpenTelemetry interoperability without a runtime dependency.

## Design

- `internal/redact`: `String(s string, secrets ...string) string` masks exact secret values, `sk-` style API keys, and `Bearer` tokens. The OpenAI adapter redacts its error text and `RawBody` with its own key before returning them. Trace output also redacts error messages from older ledgers.
- OpenAI adapter reads `usage.input_tokens`, `output_tokens`, `total_tokens` into `Message.Usage` (omitted when absent). It is part of the recorded assistant turn, so replay reproduces it.
- `internal/trace.Build(ctx, store, runID) (Trace, error)`, read-only:
  - Root span `run` with run ID, world, scenario, provider, model, principal, status, parent run and fork sequence for forks.
  - `model.invocation` spans from `model.request` to `model.response`, with requested tool calls and token usage when recorded; a provider error marks the span as error.
  - `tool.call` spans from `tool.request` to `tool.response`, with operation, call ID, HTTP status, and an error type (`transport_timeout`, `authorization_denied`, `rate_limited`, `client_error`, `server_error`). Span events inside: `authorization.check`, `fault.injected` (chaos rule or legacy 503), `retry`, `state.mutation`, `chaos.actor_mutation`.
  - Root span events: `fork` and `observation.overridden` for forks, `execution.paused`, `execution.completed`.
  - Spans that end at a restore boundary of a root run carry `checkpoint: true`.
  - An `evaluation` span with the scenario evaluation for completed runs, marked as computed at read time.
  - Durations from `recorded_at`; simulated latency from chaos events as an attribute.
  - `Summary`: counts (events, model turns, tool calls, failed tool calls, mutations, retries, faults, authorization allowed/denied, errors), wall-clock, model, and tool time, simulated latency, token totals or "not recorded", evaluation result.
- CLI:
  - `twinwright trace <run-id> [--format json|text|otlp] [--db path]`: JSON tree (default), indented text tree, or OTLP/JSON (`resourceSpans` with hex IDs, string nanosecond times, typed attribute values, a link from a fork's root span to its parent's root span).
  - `inspect` emits `summary` first, then the existing keys (run, evaluation, analysis, security, lineage, events), using an ordered struct.

## Limits

- Tool latency is the time inside one SQLite transaction, not network latency; model latency includes provider round trips only for live providers.
- Token usage is only recorded by the OpenAI adapter, which has not been exercised live.
- The OTLP output follows the OTLP JSON encoding rules but has not been sent to a collector here.
- No live metrics, sampling, or dashboards; no UI.
