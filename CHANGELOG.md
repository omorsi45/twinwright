# Changelog

Notable changes, newest first. Dates are the merge date. Versioning follows
[Semantic Versioning](https://semver.org); while the major version is 0 the
runtime schema and CLI surface may still change, and each entry says when they
do.

The database schema is versioned separately and recorded in `schema_migrations`.
`twinwright version` reports the schema version a build writes. Opening a
database written by a newer build is refused rather than downgraded.

## Unreleased

### Added

- **PostgreSQL storage backend.** `store.OpenDSN` accepts a `postgres://` URL or
  a SQLite path. Runtime SQL is written once with `?` placeholders and translated
  to `$N` by a `database/sql` driver wrapping pgx. SQLite remains the default for
  local development, deterministic examples and replay reconstruction.
  (ADR 0019, schema version 1)
- **Versioned migrations.** Schema state lives in `schema_migrations` instead of
  being inferred from `PRAGMA` probes. Each migration commits with its own
  bookkeeping row, so a failure leaves neither a partial schema nor a false
  record of success. Databases written before migration tracking are adopted in
  place.
- **Multi-worker runtime.** `twinwright run --enqueue`, `twinwright worker` and
  `twinwright queue`. Workers claim runs under fenced leases, renew them while
  executing, and take over runs whose lease lapsed. Delivery is at-least-once
  with idempotent handlers and fencing. (ADR 0020, schema version 2)
- **Transactional fencing.** The ownership check runs inside the same
  transaction as the write it protects, and rejects both a superseded fence and
  an expired lease. The monotonic committed-fence record keeps this correct under
  clock skew.
- **Ownership audit log.** `run_ownership_log` records lease acquisition,
  release, takeover, fencing rejections and run completion, separately from the
  run ledger.
- **OTLP delivery.** `twinwright trace --format otlp --otlp-endpoint <url>` posts
  spans to an OpenTelemetry collector and reports its status code. A rejection is
  an error, not a silent success. (ADR 0021)
- **Prometheus metrics.** `twinwright worker --metrics-addr` serves in-process
  worker counters plus ledger-derived gauges. Unauthenticated and off by default.
- **`twinwright version` and `twinwright doctor`.**
- **CI.** `.github/workflows/ci.yml` runs gofmt, vet, build, tests, the race
  detector, a deterministic end-to-end script, `go mod tidy` verification,
  gitleaks and govulncheck. `integration.yml` runs the suite against PostgreSQL
  16 and 17 service containers, the distributed tests under `-race`, and the
  end-to-end script against PostgreSQL.
- **Developer experience.** `Makefile`, `docker-compose.yml` for PostgreSQL and
  an OpenTelemetry collector, `scripts/e2e.sh`, `CONTRIBUTING.md`, `SECURITY.md`,
  issue and pull request templates.

### Fixed

- **A crash between persisting `model.request` and recording `model.response`
  made a run permanently unreplayable.** On resume the runner appended a second
  request, leaving more requests than responses forever. The request payload is
  derived only from the task, provider, model, transcript and exposed operations,
  so a resumed attempt regenerates it byte for byte; the runner now reuses the
  abandoned request, and fails loudly if a trailing request differs.
- **World seeds and cent amounts could overflow on PostgreSQL**, whose `INTEGER`
  is 32-bit. Those columns are `BIGINT` there.
- **Event sequence allocation was not portable**: PostgreSQL rejects a bare
  column beside an aggregate. The query groups explicitly, which is identical in
  behaviour on SQLite.
- **Two upserts incremented an unqualified column**, which PostgreSQL rejects as
  ambiguous inside `DO UPDATE SET`.
- **Fork lineage flags were written as Go booleans into integer columns**, which
  only SQLite accepts.

### Changed

- **Line endings are pinned to LF** via `.gitattributes`, so `gofmt -l` and CI
  agree with a Windows checkout.
- Read-only commands (`inspect`, `trace`, `replay`, `evaluate`, `checkpoints`,
  `compare`, `counterfactual`) accept a PostgreSQL DSN and open it with
  `default_transaction_read_only=on`, so the server refuses writes.

### Removed

- **The standalone lease package and `twinwright lease`.** ADR 0018 put leases in
  a database separate from the runs they owned, which makes fencing
  unenforceable: with no transaction spanning both, a stale worker can pass the
  check and commit anyway. Run ownership now lives in the world store. Shipping
  two lease implementations with different guarantees would be a trap for the
  next reader, so the weaker one is gone rather than deprecated in place.
  Ownership is inspected with `twinwright queue --run <run-id>`.
  (ADR 0020 supersedes ADR 0018)

## Earlier work

Milestones 1 through 14 are recorded in `docs/adr/0001` through `0018`: the world
compiler, the multi-service company world, generalized world definitions,
checkpoints and execution forks, the chaos engine, principal authorization,
declarative assertions, the counterfactual debugger, ledger-derived traces,
multiple agent providers, Twinwright Bench, experimental shadow mode, and
optional container execution.
