# Inspecting, verifying and tracing a run

This page was moved out of the project README when the README was rewritten as the Twinwright Cloud overview. It holds the README sections "Inspect and verify a run", "Run traces", "Event model", unchanged except where noted. One sentence in "Run traces" that said no live run or collector delivery had been verified now says what has been. Paths in backticks are relative to the repository root. [Back to the guide index](README.md).

## Inspect and verify a run

Use the returned run ID:

```bash
go run ./cmd/twinwright inspect <run-id> --db company.db

go run ./cmd/twinwright replay <run-id> \
  --manifest company.world.manifest.json \
  --db company.db
```

`inspect` leads with a summary (counts, wall-clock time, model time, tool time, simulated latency, token usage when a provider recorded it, and the evaluation result), then the persisted run, ledger, lineage, and analysis.

`replay` reconstructs the run in an isolated world using the recorded assistant decisions. It makes no model call and does not mutate the source database. Twinwright compares semantic events, saved tool results, transcripts, and final world state to detect divergence.

## Run traces

`trace` projects the same ledger into a span tree. It does not record a second copy of the run, and it does not change the database.

```bash
go run ./cmd/twinwright trace <run-id> --db company.db
go run ./cmd/twinwright trace <run-id> --format text --db company.db
go run ./cmd/twinwright trace <run-id> --format otlp --db company.db
```

The default is JSON. `text` is an indented tree. `otlp` is one OpenTelemetry JSON document (`resourceSpans`) with hex trace and span IDs, so a collector can ingest it. A fork's root span links to its parent's root span. The same ledger always produces the same IDs.

A scripted billing run that hits one injected 503 reads like this. Times are wall-clock commit times from the ledger, so they change between runs:

```text
run R-c4c57a885f400873b62689ff completed 38.655ms
  model.invocation scripted/fixture-v1 2.041ms
  tool.call getCustomer status=200
  model.invocation scripted/fixture-v1 2.118ms
  tool.call listInvoices status=200
  model.invocation scripted/fixture-v1 2.098ms
  tool.call listCharges status=503 server_error
    fault.injected
  model.invocation scripted/fixture-v1 2.086ms
  tool.call listCharges status=200 0.539ms
    retry
  model.invocation scripted/fixture-v1 2.084ms
  tool.call createRefund status=201 0.516ms
    state.mutation
  model.invocation scripted/fixture-v1 2.564ms
  evaluation passed
```

Each model response and each tool call is a span. Inside a tool call, the trace records authorization checks, injected faults, retries, state mutations, and chaos actor writes. The run span carries the scenario, provider, model, principal, world, and, for a fork, the parent run. An evaluation span is added for a completed run and is marked as computed when the trace is read, not as something the agent did.

Tool time is the time inside the local transaction; model time includes a live provider round trip only when the run used one; simulated chaos latency is a separate attribute rather than part of the wall clock. Token usage is recorded when the provider reports it, which scripted fixtures do not. The one live-model run recorded in this repository is the single run in `docs/live-model-run.md`. The OTLP export has been sent to a real OpenTelemetry collector and read back from Jaeger by trace ID by the `observability` job of the Integration workflow (`TestSpansAreQueryableInJaeger`); the Observability page says what that job does and does not show.

Provider error text is redacted before it is stored: the configured API key, `sk-` API keys, and bearer tokens. `trace` and `inspect` apply the same redactor to error messages already in a ledger, including `OPENAI_API_KEY` when it is set. Like `replay` and `evaluate`, they open the database read-only and still need a writable directory, because SQLite in WAL mode creates `-wal` and `-shm` files.

See `docs/adr/0012-ledger-traces.md`.

## Event model

Twinwright records an ordered run-scoped ledger containing execution events such as:

```text
execution.started
model.request
model.response
tool.request
authorization.allowed
authorization.denied
tool.response
state.mutation
error
retry
execution.paused
execution.completed
execution.forked
observation.overridden
```
