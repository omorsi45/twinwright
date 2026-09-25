# ADR 0012: Ledger traces

Status: accepted, 2026-09-25

A run's ledger is complete enough to explain it, but `inspect` dumped that ledger before any summary, and nothing rendered the call tree. The roadmap asks for structured observability, with OpenTelemetry where practical, and for secret redaction before more integrations.

Emitting live OpenTelemetry spans from the runtime was rejected for this milestone. It adds a collector dependency, and a crash drops spans that the ledger already keeps. The ledger stays the source of truth. `trace` projects it, at read time, into a span tree: one run span, a model invocation for each request and response, and a tool call for each request and response. Authorization checks, faults, retries, state mutations, and actor writes are events on the tool span. A fork records its parent on the run span. Checkpoints are an attribute on spans that end at a restore boundary of a root run. A completed run gets an evaluation span marked as computed when the trace is read.

Span and trace IDs are hashes of the run ID and the starting event sequence, so the same ledger always yields the same trace. Durations use `recorded_at`, the wall-clock commit time. Sums are rounded to milliseconds so binary float noise does not show up in JSON. Simulated chaos latency stays an attribute. Token counts are stored on the assistant turn when the OpenAI adapter sees `usage` in the response, and the trace adds them up. Scripted fixtures record none.

`inspect` now leads with that summary, using a struct so the key is not sorted under the raw events. `trace --format otlp` writes one OTLP JSON document. A fork links to the parent root span, whose ID is the same hash the parent trace uses, so the parent does not have to be loaded. The document has not been delivered to a collector here.

Provider failures used to store the raw HTTP body. The OpenAI adapter now runs that text, and the error returned to the runner, through a redactor that masks the configured API key, `sk-` keys, and bearer tokens. `trace` and `inspect` apply the same redactor to error messages already in a ledger, and they also mask `OPENAI_API_KEY` when that variable is set. The redactor does not try to detect every secret shape. A simulated permission revocation is labeled `permission_revocation` rather than a generic client error, and an actor mutation keeps the scalar fields of the write it performed.

There is no metrics pipeline, no sampling, and no UI. Tool time is local transaction time. Model time includes a network round trip only for a live provider, and no live run has been verified.
