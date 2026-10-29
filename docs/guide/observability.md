# Observability

This page was moved out of the project README when the README was rewritten as the Twinwright Cloud overview. It holds the README section "Observability", unchanged except where noted. The sentence about what `docker compose up -d` starts was corrected: the collector forwards to Jaeger, and Prometheus is part of the stack. A delivery to a collector that answers 200 and reports rejected spans is described as a failed one, and the paragraph on worker metrics says that a worker whose address cannot be bound exits. The sections "Exporting from a worker", "Metrics", "The Compose stack" and "What CI shows" at the end are new, and so is the paragraph on collector credentials. Paths in backticks are relative to the repository root. [Back to the guide index](README.md).

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
is an error, not a silent success. That includes a collector that answers 200 and
reports `partialSuccess.rejectedSpans` above zero: the delivery report then says
`"delivered": false` with a `rejected_spans` count, the command exits non-zero, and
a worker logs it at warn like any other failed export
(`TestExporterReportsAPartialSuccess`, `TestTraceCommandFailsOnAPartialSuccess`,
`TestWorkerWarnsOnAPartialSuccess`). `docker compose up -d` starts a collector that
prints what it receives and forwards it to Jaeger, plus a Prometheus that scrapes workers
(the stack is described under "The Compose stack" below).

A worker exposes Prometheus metrics with `--metrics-addr`: in-process counters
for claims, dispositions, takeovers, fencing rejections and execution seconds,
plus gauges read from the database at scrape time for queue depth, run statuses,
tool calls, retries, authorization denials, chaos injections and ownership
transitions. **The endpoint is
unauthenticated and off by default: bind it to loopback.** The address is bound before the worker registers or claims anything: if it cannot be bound (another process holds the port, say) the worker exits with an error naming `--metrics-addr` and the address, instead of running without the metrics it was asked for (`TestWorkerFailsWhenTheMetricsAddressIsTaken`). See
`docs/adr/0021-otlp-and-metrics.md`.

### Exporting from a worker

`twinwright worker` accepts `--otlp-endpoint` and `--otlp-header`. Unlike `trace`, it does not default the endpoint from `OTEL_EXPORTER_OTLP_ENDPOINT`, so export is off unless asked for. When a claim ends `finished` on a `completed` run, the worker builds that run's trace and exports it in the background under a 10 second bound, and the `claim finished` log line carries the `trace_id`. A paused, failed, requeued or fenced claim does not export. Export is best effort: a failure is logged at warn level with the trace ID and with credentials scrubbed from the text, and never changes the claim's disposition (`TestWorkerExportFailureDoesNotFailTheRun`, `TestWorkerExportFailureWarnsWithoutLeakingCredentials`). Nothing about the export reaches the ledger (`TestWorkerExportsACompletedRunsTrace` drains the same scripted run with and without export and requires the same event types). Delivery is still at-least-once: a run delivered twice can be exported twice, to the same trace ID.

**The export runs behind the claim loop, one at a time.** A collector that does not answer would otherwise hold the worker for the whole 10 seconds after every completed run. Now the next claim does not wait (`TestExportDoesNotDelayTheNextClaim`). A worker has at most one export in flight; a run that finishes while one is still running is not exported, and the worker logs `trace export dropped` at warn with that run's `trace_id`. Send it later with `twinwright trace <run-id> --format otlp --otlp-endpoint ...`, since a trace is derived from the ledger. With a healthy collector an export takes milliseconds, so a drop needs two runs to finish within that time of each other. When the worker is told to stop, the export in flight is not cancelled: `Serve` and the one-shot `--drain` and `--max-runs` modes wait for it, for at most 10 seconds, before they return (`TestStopDoesNotCancelTheExportInFlight`, `TestServeWaitsForTheExportInFlightBeforeItReturns`, `TestCloseGivesUpOnAnExportThatIgnoresItsContext`).

Because the outcome is returned before the export ends, it cannot say whether the collector took the trace. The `trace_export_queued` field of an outcome (it replaces `trace_exported`, which was unreleased) says only that the trace was handed to the exporter. The result is in the log: `trace exported` at info when the collector accepted all of it, `trace export failed` at warn otherwise.

Every exported span carries `twinwright.event.start_seq`, `twinwright.event.end_seq` and `twinwright.run_id`, so a reader of the collector can name the ledger events behind a span (`TestOTLPSpansCarryLedgerSequence`). `trace --otlp-endpoint` reports the `trace_id` it delivered (`TestDeliveryReportsTheTraceID`).

**Collector credentials.** A collector behind an auth proxy needs a header, and a hosted one often wants a token in the endpoint's userinfo or query. Both `trace` and `worker` send headers from `--otlp-header "Name: value"` and, when that flag is not given, from the standard `OTEL_EXPORTER_OTLP_HEADERS` variable: comma-separated `name=value` pairs whose values are percent-decoded, for example `OTEL_EXPORTER_OTLP_HEADERS='Authorization=Bearer%20TOKEN,X-Tenant=acme'` (`TestResolveOTLPHeaders`, `TestTraceCommandReadsTheOTLPHeaderFromTheEnvironment`, `TestWorkerReadsTheOTLPHeaderFromTheEnvironment`). **Prefer the variable for a worker.** An `--otlp-header` value is in the worker's command line for as long as it runs, where any user on the host can read it from the process listing; an environment variable is not. When both are given the flag wins. A malformed variable is refused before the database is opened, without repeating its values (`TestWorkerRejectsAMalformedOTLPHeadersVariableBeforeAnyDatabaseWork`). Header values never appear in the delivery report, in an error or in a log line.

The endpoint is reported without its userinfo and query: the `endpoint` field of the delivery report and every error name the address alone, and a collector that echoes the credentials back in its refusal has them masked (`TestTraceCommandExportDoesNotPrintEndpointCredentials`, `TestExporterNeverReturnsTheEndpointCredentials`). A query string on the endpoint is kept on the request and the traces path is added to the endpoint's path, so `http://host:4318/?token=x` posts to `/v1/traces?token=x` (`TestExporterKeepsTheQueryOfTheEndpoint`).

### Metrics

The worker exposes a run-duration histogram (`twinwright_worker_run_duration_seconds`), the age of the oldest runnable queue entry (`twinwright_queue_oldest_runnable_age_seconds`) and the time since each registered worker was last seen (`twinwright_worker_last_seen_age_seconds`), next to the counters and gauges above. The age gauge answers for what a worker could claim right now, by the same predicate `ClaimRun` selects with: a runnable entry counts from when it became available, an entry whose lease expired (a crashed worker's run) counts from the expiry, and an entry in backoff or a run that another worker's live lease still holds does not count (`TestOldestRunnableAgeCountsAnExpiredLease`, `TestOldestRunnableAgeIgnoresARunHeldByALiveLease`, `TestOldestClaimableSince`). Every counter renders a zero sample from the first scrape, so `rate()` has a starting point, and each family is contiguous as the Prometheus text format requires (`TestExpositionIsWellFormed`, `TestCountersRenderZeroBeforeFirstUse`). The ownership gauge renders every kind the log can hold at zero, and the ledger-event gauge the four types the endpoint names (`tool.response`, `retry`, `authorization.denied`, `chaos.injected`), so the first fencing rejection or takeover is a step from 0 to 1 and not a new series that `increase()` cannot see (`TestOwnershipEventsRenderEveryKindAtZero`). Other ledger event types appear with their first event.

**Run durations never go backwards.** A claim is timed on the process's monotonic clock, not on the clock leases are stamped with, so a clock step during a run (NTP, a VM migration) cannot make `twinwright_worker_run_seconds_total` or the histogram's `_sum` decrease; Prometheus would read that as a counter reset. A counter ignores a negative or non-finite delta and the histogram ignores a negative or non-finite observation (`TestClaimDurationIgnoresABackwardClockStep`, `TestCountersRefuseNegativeDeltas`, `TestRunDurationIgnoresNegativeObservations`).

**Which workers are listed.** `twinwright_workers_registered` and `twinwright_worker_last_seen_age_seconds` list only workers seen within the last **ten minutes** (`metrics.DefaultWorkerWindow`; their HELP text says so), and the count and the series use the same set (`TestLastSeenAgeOmitsPrunedWorkers`). A worker is seen when it registers, claims a run, renews a lease, or polls an empty queue, which it does at most once a minute, so a healthy idle worker stays inside the window (`TestAnIdleWorkerStaysSeen`). A worker that stops cleanly (`Serve` returning, or `worker --drain` and `--max-runs` ending) removes its own row, unless it still owns a live lease (`TestCloseRemovesTheWorkersRow`, `TestServeRemovesItsRowOnACleanStop`, `TestRemoveWorker`). A worker that is killed leaves its row, which `twinwright queue` keeps listing, and drops out of the metrics ten minutes after its last sign of life. So an alert on last-seen age fires for a stopped worker for ten minutes at most, and the number of series is bounded by the workers running in that window and not by every restart.

**The claim counters add up.** `twinwright_worker_claims_total` equals the sum of the `twinwright_worker_claim_disposition_total` series. A claim settled without being executed because its run was past `--max-attempts` is timed like any other (`TestOverDeliveredClaimIsTimed`). A write that ends a claim and is refused by the fence counts as `fenced`, adds a fencing rejection and is noted in the ownership log, not as `finished` (`TestClaimRefusedByTheFenceAtTheEndIsFenced`). A claim whose closing write fails for any other reason ended in no disposition: the worker returns the error, the run's lease lapses and it is delivered again, and the claim is in neither counter (`TestClaimWhoseClosingWriteFailsIsNotObservedAsFinished`, `TestOverDeliveredClaimWithAnUnreadableRunIsNotObserved`).

**The database-derived gauges are served by every worker for the database it points at.** With several workers on one database, scrape one of them or aggregate with `max`, never `sum`: a sum multiplies the queue depth by the number of workers. ADR 0032 lists this among its limits.

### The Compose stack

`docker compose up -d` starts PostgreSQL, an OpenTelemetry collector (OTLP/HTTP on 127.0.0.1:4318, health check on 13133), Jaeger 2.22.0 (UI and query API on 127.0.0.1:16686) and Prometheus 3.15.1 (127.0.0.1:9090), all published on loopback. The collector prints what it receives and forwards it to Jaeger. Prometheus scrapes workers started with `--metrics-addr 0.0.0.0:9095` and `0.0.0.0:9096` through `host.docker.internal`, because a container reaches the host through the bridge gateway and a loopback listener does not answer there. So a worker Prometheus scrapes listens on all interfaces; that is an exception to "bind it to loopback", and it is safe only on a machine whose ports other people cannot reach. **None of the four services has TLS or authentication.**

### What CI shows

The `observability` job of the Integration workflow starts the collector, Jaeger and Prometheus and runs two opt-in tests (`TWINWRIGHT_TEST_OTEL=1`). `TestSpansAreQueryableInJaeger` exports a scripted `company-incident` run through the collector and reads it back from Jaeger by trace ID, requiring the same span count, a root with the local root's name and a span carrying `twinwright.event.start_seq`. `TestPrometheusScrapesAWorker` serves a worker's metrics and requires Prometheus to return a `runnable` queue entry count of 1 and the worker's claims counter, both from the test's own listener (`host.docker.internal:9095`), not from the other target in `deploy/prometheus.yml`. They call no model. The job fails if either test skips or does not run: its step requires a `--- PASS:` line for each test by name, so a skip, a renamed test or a renamed environment variable cannot leave it green. They show the bytes are accepted and queryable by these two tools at the pinned versions; they say nothing about another collector or backend. The job passed on its first run on 2026-10-28 (run 40730837862) and on every Integration run since, among them run 40814472427, the first with the `--- PASS:` requirement. See `docs/adr/0032-observability-verified.md`.
