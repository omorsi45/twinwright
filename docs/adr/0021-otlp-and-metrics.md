# ADR 0021: OTLP delivery and runtime metrics

Status: accepted, 2026-09-26; extends ADR 0012

ADR 0012 made the durable ledger the source of truth for traces and added a
formatter that prints an OTLP document. Printing a document is not exporting it,
and a trace without counters answers "what happened in this run" but not "what
is this fleet doing". This ADR closes both gaps without introducing a second
source of truth.

## OTLP delivery over HTTP, without the OpenTelemetry SDK

`twinwright trace --format otlp --otlp-endpoint http://host:4318` posts the
document to a collector and reports the collector's status code. A non-2xx
response is an error, because an exporter that reported success on a 403 would
convert a misconfigured collector into silent data loss - the exact failure mode
observability work exists to prevent.

*Rejected: depending on the OpenTelemetry Go SDK.* Twinwright's spans are
derived from the ledger **after the fact**, not emitted live from instrumented
code. The SDK's span lifecycle, context propagation, samplers and batch
processors would all be unused, and its tracer-provider lifecycle would have to
be threaded through commands that only read a database. What matters is that the
bytes reach a real collector in the documented wire format, which a plain HTTP
POST does. If Twinwright ever emits spans live from a long-running service, that
trade-off should be revisited.

A consequence worth stating: because traces are reconstructed from the ledger, a
trace can be exported long after the run finished, re-exported after a collector
outage, and produced identically on any machine holding the database. Live
instrumentation cannot do any of those.

## Metrics: in-process counters, ledger-derived gauges

`twinwright worker --metrics-addr 127.0.0.1:9095` serves the Prometheus text
format. The split between the two kinds of number is deliberate.

**In-process counters** describe events in this worker: claims, claim
dispositions, takeovers, fencing rejections, execution seconds. An event that has
already happened cannot be recovered from current state, so it must be counted as
it occurs.

**Gauges are read from the database at scrape time**: queue depth by state, runs
by status, and ledger-derived counts (tool responses, retries, authorization
denials, chaos injections, ownership transitions, registered workers). These are
computed from the ledger on every scrape rather than maintained as separate
counters, which keeps the ledger the single source of truth. A separately
maintained counter can disagree with the ledger; a query cannot.

Every series is emitted even when its value is zero. A series that vanishes at
zero breaks the alert that matters most - "the queue drained" or "denials
stopped" - because the scraper sees a gap rather than a zero.

A failed scrape returns HTTP 500 rather than a partial document. Half a document
parses as every missing series dropping to zero, which reads as an outage that is
not happening.

*Rejected: a Prometheus client library.* The text exposition format is a stable,
documented, line-oriented contract and a scraper only needs correct bytes. The
library's registry, collector interfaces and default Go/process collectors would
add a dependency to produce output this package produces in a few hundred lines,
and the bytes are asserted directly in tests.

*Rejected: a bespoke monitoring UI.* A weak dashboard is worse than none: it
implies a level of operational maturity that a hand-rolled page does not have.
The repository ships a docker-compose collector configuration instead, so anyone
can point real tooling at it.

## Security boundary

The metrics endpoint is unauthenticated and off by default. It exposes queue
depth, run counts and ownership topology, so the flag's documentation says to
bind loopback, and the compose file publishes its ports on 127.0.0.1 only. The
code binds exactly where the operator asks rather than silently rewriting the
address, because quietly changing a bind address is its own kind of surprise; the
constraint is therefore documented rather than enforced.

## What is not claimed

There is no metrics push, no exemplars, no histograms (durations are exposed as
counters of seconds, which is honest about what is measured), and no live
provider cost accounting beyond the token usage a provider actually reports. Live
delivery to a collector is exercised against a real HTTP server in tests; the
docker-compose collector is offered for manual confirmation and is not part of
CI.
