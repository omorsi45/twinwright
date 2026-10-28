# 0032. The observability stack, and what the CI job proves

Date: 2026-10-28

Status: accepted; extends ADR 0012 and ADR 0021

## Context

ADR 0021 delivered traces to a collector and served metrics to a scraper, and
tested both against servers started inside the test. That shows the bytes are
sent and that the document has the documented shape. It does not show that a real
collector accepts what the exporter posts, that a backend behind it can give the
spans back, or that a real Prometheus accepts the metrics document. ADR 0021 said
so itself: the Compose collector was "offered for manual confirmation and is not
part of CI".

The surfaces also changed under it. Every exported span now carries the ledger
sequence numbers it covers, a worker can export the trace of a run it finishes,
and the metrics document was reshaped so every family is contiguous and every
counter has a zero sample from the first scrape. Those are the changes a real
consumer would notice first.

This ADR records the stack that gives those claims a real consumer, the two tests
that use it, the CI job that runs them, and what they do and do not show.

## Decision

### The stack

`docker compose up -d otel-collector jaeger prometheus` starts three services. All
published ports are bound to `127.0.0.1`.

| Service | Image (pinned) | Published | Role |
| --- | --- | --- | --- |
| `otel-collector` | `otel/opentelemetry-collector-contrib:0.111.0` | 4318 (OTLP/HTTP), 13133 (health) | receives spans, prints them with the `debug` exporter, forwards them to Jaeger |
| `jaeger` | `jaegertracing/jaeger:2.22.0` | 16686 (UI and query API) | stores the spans and answers `GET /api/traces/{traceID}` |
| `prometheus` | `prom/prometheus:v3.15.1` | 9090 | scrapes a worker's metrics and answers queries |

The collector's configuration is `deploy/otel-collector.yaml`: an OTLP/HTTP
receiver on 4318, a `batch` processor with a one second timeout, the `debug`
exporter, an `otlp/jaeger` exporter to `jaeger:4317` with TLS switched off, and the
`health_check` extension on 13133. Jaeger's own OTLP port is not published; only the
collector reaches it, over the Compose network, and Jaeger runs the image's default
configuration. The collector `depends_on` Jaeger, which orders the start and does
not wait for Jaeger to be ready; the exporter retries.

Prometheus is configured by `deploy/prometheus.yml`: a two second scrape interval
and one job, `twinwright-workers`, with the static target
`host.docker.internal:9095`. The service maps that name with
`extra_hosts: host.docker.internal:host-gateway`, so a worker running on the host
with `--metrics-addr` on port 9095 is scraped.

The default test suite starts none of this, and `docker compose up -d` is still
optional for development. It now starts all four services, PostgreSQL included.

### Two opt-in tests

Both skip unless `TWINWRIGHT_TEST_OTEL=1`, and say so, so `go test ./...` stays
hermetic.

`TestSpansAreQueryableInJaeger` (`internal/trace/collector_test.go`) builds a
scripted `company-incident` run on SQLite, exports it with `trace.Exporter` to
`TW_OTLP_ENDPOINT` (default `http://127.0.0.1:4318`), and polls
`GET {TW_JAEGER_URL}/api/traces/{traceID}` (default `http://127.0.0.1:16686`) every
500 ms for up to 60 s. It passes when the answer holds the same number of spans as
the trace built locally, exactly one span without a `CHILD_OF` reference whose
operation name is the local root's name, and at least one span with the tag
`twinwright.event.start_seq`.

`TestPrometheusScrapesAWorker` (`internal/metrics/prometheus_test.go`) serves
`metrics.Serve` on `0.0.0.0:9095` over a store holding one queued run, and polls
`GET {TW_PROMETHEUS_URL}/api/v1/query` (default `http://127.0.0.1:9090`) every
500 ms for up to 60 s. It passes when `twinwright_queue_entries` has a sample with
`state="runnable"` and value 1, and `twinwright_worker_claims_total` has a sample
labelled with the test worker's ID, which exists at 0 because every counter
renders before its first use.

On a timeout, each fails with the last problem found and the last answer, cut to
4 KiB, so a CI failure can be diagnosed from its log.

The comparison of Jaeger's answer with the local trace, the reading of
Prometheus's answer, and the polling around both are also covered by ordinary tests
against canned answers: a wrong span count, a missing or doubled root, a link
mistaken for a parent, a missing tag, a wrong queue value, a missing counter, a
listener that died and an answer longer than the failure message are each refused.
Without them the assertions would only ever have run in CI.

### The CI job

The `observability` job in `.github/workflows/integration.yml` runs on
`ubuntu-latest` with a 20 minute limit. It starts the three services, waits (up to
60 tries, two seconds apart) for `http://127.0.0.1:13133/` and
`http://127.0.0.1:16686/`, and runs

```
TWINWRIGHT_TEST_OTEL=1 go test -count=1 -v -run 'TestSpansAreQueryableInJaeger|TestPrometheusScrapesAWorker' ./internal/trace/ ./internal/metrics/
```

A failed job prints `docker compose logs`, and `docker compose down -v` runs
whatever happened. **The CI job is the evidence for what follows. This ADR does not
claim it has passed**: the run is recorded where the job's results are, not here.

### Trace events keep an interruption's status and wait

ADR 0031 listed as a limit that a span event for a `provider.interrupted` payload
kept `kind`, `attempt`, `reason` and `deadline_ms` and dropped `status` and
`retry_after_ms`. It now keeps both when the payload has them, so a rate limit's
status and wait reach the trace. A payload without them, such as a scripted or timed
out attempt, produces the event it always did
(`TestBuildKeepsTheStatusAndWaitOfAProviderInterruption`).

## What is proven

When the job passes, for one scripted run and the pinned images:

- A real collector accepts the OTLP JSON `trace.Exporter` posts, forwards it, and
  Jaeger returns the trace by the trace ID `trace.TraceIDFor` derives, with every
  span, one root with the local root's name, and the ledger sequence tag.
- A real Prometheus scrapes the metrics endpoint `metrics.Serve` serves and returns
  its series to a query. A scrape Prometheus rejects, for any reason, leaves those
  series missing and fails the job.

## Limits

- **No TLS and no authentication anywhere on the stack.** The OTLP receiver, the
  health endpoint, Jaeger's UI and query API, Prometheus and the worker's metrics
  endpoint are plain HTTP. Compose publishes everything on loopback only, and
  Jaeger's OTLP port is not published, but a loopback bind is the only access
  control there is.
- **The gated metrics listener binds `0.0.0.0` in CI only.** The Prometheus
  container reaches the host through the bridge gateway, which a loopback-only
  listener does not answer. The bind lasts for one test and serves a throwaway
  store with one queued run. It is not a recommendation for a worker: the endpoint
  is unauthenticated and exposes queue and run topology.
- **Database-derived gauges are served by every worker.** Queue depth, run
  statuses, ledger counts and worker ages describe the database a worker points
  at, so several workers on one database serve the same values. Scrape one of them,
  or aggregate with `max`; `sum` multiplies every such gauge by the worker count.
  The job scrapes one worker and does not test a multi-worker setup.
- **Trace export from a worker is best effort.** A failure is logged and never
  changes a claim's disposition. The job exports with `trace.Exporter` directly; it
  does not start a worker with `--otlp-endpoint`, whose wiring is covered by tests
  against an in-process server.
- **Nothing about traces or metrics is written to the event ledger.** The ledger
  records the agent's trajectory and never a worker's identity, and an export or a
  scrape leaves it as it was.
- **Delivery stays at-least-once.** A run delivered twice can be exported twice, to
  the same trace ID. Whether Jaeger collapses a re-exported span was not examined,
  and the test exports once, so its exact span count does not depend on it.
- **One shape of trace.** A completed scripted run with no fork. A fork's link to
  its parent's trace, a failed run and a paused run are not read back from Jaeger.
- **Only the pinned versions.** Other collector, Jaeger or Prometheus versions are
  not verified. Images are pulled from Docker Hub without credentials, so a pull
  limit can fail the job before a test runs. Jaeger's retention was not examined;
  treat the stack as disposable. Timings on a cold runner were not measured, and 60
  seconds is a bound chosen for headroom, not a measurement.
- ADR 0021's statement that the Compose collector is not part of CI no longer holds.
  The rest of that ADR stands.

## Consequences

- A change to the OTLP encoding, the span attributes or the exposition document is
  checked against real software on every integration run.
- The pinned versions are part of what is verified. Changing one is a change to the
  job's evidence and is reviewed with it.
- `docker compose up -d` pulls and starts two more services than before. Name the
  services to start fewer.

## Corrections

- 2026-10-28: the exporter returned the collector endpoint as it was given, so `trace --otlp-endpoint` printed the endpoint's userinfo and query in its delivery report and in its errors, which the worker's log line had always scrubbed. `Delivery.Endpoint` and every error the exporter returns now name the address without userinfo or query, and mask header values and the endpoint's own credentials if a collector echoes them back. An endpoint with a query string was posted to the wrong path and now keeps its query. Collector headers can be set with the standard `OTEL_EXPORTER_OTLP_HEADERS` variable (comma-separated `name=value` pairs, values percent-decoded) on both `trace` and `worker` when `--otlp-header` is not given, so a worker's credential need not sit in its command line.
