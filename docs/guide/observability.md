# Observability

This page was moved out of the project README when the README was rewritten as the Twinwright Cloud overview. It holds the README section "Observability", unchanged except where noted. One sentence about the collector was corrected. The sections "Exporting from a worker", "Metrics", "The Compose stack" and "What CI shows" at the end are new. Paths in backticks are relative to the repository root. [Back to the guide index](README.md).

## Observability

Traces are derived from the durable ledger, so they can be produced long after a
run finished and identically on any machine holding the database.

```bash
go run ./cmd/twinwright trace <run-id> --db twinwright.db                       # JSON
go run ./cmd/twinwright trace <run-id> --format text --db twinwright.db         # tree
go run ./cmd/twinwright trace <run-id> --format otlp --db twinwright.db         # OTLP document
go run ./cmd/twinwright trace <run-id> --format otlp \
  --otlp-endpoint http://127.0.0.1:4318 --db twinwright.db                      # deliver it
```

The endpoint form POSTs to the collector and reports its status code; a rejection
is an error, not a silent success. `docker compose up -d` starts a collector that
prints what it receives and forwards it to Jaeger, plus a Prometheus that scrapes workers
(the stack is described under "The Compose stack" below).

A worker exposes Prometheus metrics with `--metrics-addr`: in-process counters
for claims, dispositions, takeovers, fencing rejections and execution seconds,
plus gauges read from the database at scrape time for queue depth, run statuses,
tool calls, retries, authorization denials, chaos injections and ownership
transitions. **The endpoint is
unauthenticated and off by default: bind it to loopback.** See
`docs/adr/0021-otlp-and-metrics.md`.

### Exporting from a worker

`twinwright worker` accepts `--otlp-endpoint` and `--otlp-header`. Unlike `trace`, it does not default the endpoint from `OTEL_EXPORTER_OTLP_ENDPOINT`, so export is off unless asked for. When a claim ends `finished` on a `completed` run, the worker builds that run's trace and exports it under a 10 second bound, and the `claim finished` log line carries the `trace_id`. A paused, failed, requeued or fenced claim does not export. Export is best effort: a failure is logged at warn level with the trace ID and with credentials scrubbed from the text, and never changes the claim's disposition (`TestWorkerExportFailureDoesNotFailTheRun`, `TestWorkerExportFailureWarnsWithoutLeakingCredentials`). Nothing about the export reaches the ledger (`TestWorkerExportsACompletedRunsTrace` drains the same scripted run with and without export and requires the same event types). Delivery is still at-least-once: a run delivered twice can be exported twice, to the same trace ID.

Every exported span carries `twinwright.event.start_seq`, `twinwright.event.end_seq` and `twinwright.run_id`, so a reader of the collector can name the ledger events behind a span (`TestOTLPSpansCarryLedgerSequence`). `trace --otlp-endpoint` reports the `trace_id` it delivered (`TestDeliveryReportsTheTraceID`).

**Collector credentials.** A collector behind an auth proxy needs a header, and a hosted one often wants a token in the endpoint's userinfo or query. Both `trace` and `worker` send headers from `--otlp-header "Name: value"` and, when that flag is not given, from the standard `OTEL_EXPORTER_OTLP_HEADERS` variable: comma-separated `name=value` pairs whose values are percent-decoded, for example `OTEL_EXPORTER_OTLP_HEADERS='Authorization=Bearer%20TOKEN,X-Tenant=acme'` (`TestResolveOTLPHeaders`, `TestTraceCommandReadsTheOTLPHeaderFromTheEnvironment`, `TestWorkerReadsTheOTLPHeaderFromTheEnvironment`). **Prefer the variable for a worker.** An `--otlp-header` value is in the worker's command line for as long as it runs, where any user on the host can read it from the process listing; an environment variable is not. When both are given the flag wins. A malformed variable is refused before the database is opened, without repeating its values (`TestWorkerRejectsAMalformedOTLPHeadersVariableBeforeAnyDatabaseWork`). Header values never appear in the delivery report, in an error or in a log line.

The endpoint is reported without its userinfo and query: the `endpoint` field of the delivery report and every error name the address alone, and a collector that echoes the credentials back in its refusal has them masked (`TestTraceCommandExportDoesNotPrintEndpointCredentials`, `TestExporterNeverReturnsTheEndpointCredentials`). A query string on the endpoint is kept on the request and the traces path is added to the endpoint's path, so `http://host:4318/?token=x` posts to `/v1/traces?token=x` (`TestExporterKeepsTheQueryOfTheEndpoint`).

### Metrics

The worker exposes a run-duration histogram (`twinwright_worker_run_duration_seconds`), the age of the oldest runnable queue entry (`twinwright_queue_oldest_runnable_age_seconds`) and the time since each registered worker was last seen (`twinwright_worker_last_seen_age_seconds`), next to the counters and gauges above. The age gauge answers for what a worker could claim right now, by the same predicate `ClaimRun` selects with: a runnable entry counts from when it became available, an entry whose lease expired (a crashed worker's run) counts from the expiry, and an entry in backoff or a run that another worker's live lease still holds does not count (`TestOldestRunnableAgeCountsAnExpiredLease`, `TestOldestRunnableAgeIgnoresARunHeldByALiveLease`, `TestOldestClaimableSince`). Every counter renders a zero sample from the first scrape, so `rate()` has a starting point, and each family is contiguous as the Prometheus text format requires (`TestExpositionIsWellFormed`, `TestCountersRenderZeroBeforeFirstUse`).

**Run durations never go backwards.** A claim is timed on the process's monotonic clock, not on the clock leases are stamped with, so a clock step during a run (NTP, a VM migration) cannot make `twinwright_worker_run_seconds_total` or the histogram's `_sum` decrease; Prometheus would read that as a counter reset. A counter ignores a negative or non-finite delta and the histogram ignores a negative or non-finite observation (`TestClaimDurationIgnoresABackwardClockStep`, `TestCountersRefuseNegativeDeltas`, `TestRunDurationIgnoresNegativeObservations`).

**The database-derived gauges are served by every worker for the database it points at.** With several workers on one database, scrape one of them or aggregate with `max`, never `sum`: a sum multiplies the queue depth by the number of workers. ADR 0032 lists this among its limits.

### The Compose stack

`docker compose up -d` starts PostgreSQL, an OpenTelemetry collector (OTLP/HTTP on 127.0.0.1:4318, health check on 13133), Jaeger 2.22.0 (UI and query API on 127.0.0.1:16686) and Prometheus 3.15.1 (127.0.0.1:9090), all published on loopback. The collector prints what it receives and forwards it to Jaeger. Prometheus scrapes workers started with `--metrics-addr 0.0.0.0:9095` and `0.0.0.0:9096` through `host.docker.internal`, because a container reaches the host through the bridge gateway and a loopback listener does not answer there. So a worker Prometheus scrapes listens on all interfaces; that is an exception to "bind it to loopback", and it is safe only on a machine whose ports other people cannot reach. **None of the four services has TLS or authentication.**

### What CI shows

The `observability` job of the Integration workflow starts the collector, Jaeger and Prometheus and runs two opt-in tests (`TWINWRIGHT_TEST_OTEL=1`). `TestSpansAreQueryableInJaeger` exports a scripted `company-incident` run through the collector and reads it back from Jaeger by trace ID, requiring the same span count, a root with the local root's name and a span carrying `twinwright.event.start_seq`. `TestPrometheusScrapesAWorker` serves a worker's metrics and requires Prometheus to return a `runnable` queue entry count of 1 and the worker's claims counter. They call no model. The job fails if either test skips or does not run: its step requires a `--- PASS:` line for each test by name, so a skip, a renamed test or a renamed environment variable cannot leave it green. They show the bytes are accepted and queryable by these two tools at the pinned versions; they say nothing about another collector or backend. See `docs/adr/0032-observability-verified.md`.
