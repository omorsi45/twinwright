# ADR 0019: PostgreSQL storage backend

Status: accepted, 2026-09-26

ADR 0017 froze the single-node guarantees and required that any distributed
design map each one onto its new storage before claiming production readiness.
This ADR adds the storage half: a PostgreSQL-backed world store that runs the
same runtime code as SQLite. ADR 0020 adds the worker half.

## Decision

`store.OpenDSN` accepts either a `postgres://` URL or a local SQLite file path.
SQLite stays the default for local development, deterministic examples, replay
reconstruction and evaluation; PostgreSQL is what the multi-worker runtime uses.

Three things make one body of runtime SQL serve both backends.

**Statement translation at the driver, not at every call site.** Behavior
handlers, chaos, authorization, checkpoint reconstruction and replay all receive
a `*sql.Tx` and write `?`-placeholder SQL. `internal/pgsql` registers a
`database/sql` driver that wraps pgx and rewrites `?` to `$N` lexically,
skipping string literals, quoted identifiers, dollar-quoted bodies and nested
comments.

*Rejected: duplicating every statement per dialect.* Over three hundred call
sites across twenty packages would each grow a second copy that drifts silently;
a dialect bug would then be a behaviour difference rather than a syntax error.

*Rejected: a query-builder abstraction.* It would rewrite working, reviewed SQL
for no runtime benefit and hide the actual statements from anyone auditing the
transaction boundaries, which are the part that matters.

*Limitation, stated because the rewriter cannot detect it:* every `?` outside a
literal or comment is treated as a placeholder, so PostgreSQL's jsonb `?`
existence operator is unavailable in translated SQL. Twinwright does not use
it, and dialect-specific SQL written with native `$N` parameters passes through
untouched.

**Explicit integer widths.** SQLite's `INTEGER` is 64-bit; PostgreSQL's is
32-bit. World seeds and cent amounts are Go `int64`, so the PostgreSQL schema
uses `BIGINT`. A test seeds a world with 5,000,000,000 and reads it back to
prove the round trip, because this class of bug is invisible until a large value
appears in production.

**Portable aggregates.** PostgreSQL rejects a bare column beside an aggregate.
Event sequence allocation now groups explicitly; the grouped form is identical
on SQLite because the join yields at most one world row per run.

Timestamps stay `TEXT` in RFC 3339 on both backends rather than becoming
`timestamptz`. Determinism is the reason: the ledger's recorded and world clocks
are compared byte-for-byte during replay, and a backend that normalises,
rounds or re-zones a timestamp would make a PostgreSQL run and a SQLite run of
the same fixture diverge for no semantic reason.

## Migrations

Schema state is now versioned in `schema_migrations` instead of inferred from
`PRAGMA table_info` probes. Each migration runs inside one transaction together
with the row recording it, so a failure leaves neither a half-applied schema nor
a false record of success. Both backends support transactional DDL, which is
what makes that a guarantee rather than an intention.

The base migration is written to adopt a database created before migration
tracking existed: every statement is `IF NOT EXISTS` and every column addition
is guarded, so an old SQLite file upgrades in place with its runs intact. A
database recording a version *newer* than the running build is refused rather
than opened, because silently operating on an unknown schema is how runtime
history gets corrupted.

## Guarantees mapped from ADR 0017

- *World isolation*: unchanged. Every world query is still scoped by
  `world_id`; a PostgreSQL test mutates one seed instance and proves its sibling
  is untouched.
- *Atomic local mutation with ledger effects*: unchanged. The dispatcher's
  single transaction now spans a PostgreSQL transaction instead of a SQLite one.
  PostgreSQL's default `READ COMMITTED` is at least as strong as SQLite's
  serialised single writer for this pattern, because every conflicting write in
  the runtime targets the same row and takes a row lock.
- *Tool call idempotency*: unchanged, and now tested directly on PostgreSQL:
  the same call ID returns the saved result and commits no second refund, while
  the same call ID with different arguments is refused.
- *Replay source safety*: unchanged. Replay still reconstructs into a
  throwaway local SQLite database rather than writing to the source, so
  verification cannot mutate a shared PostgreSQL runtime.
- *Deterministic initialization*: unchanged. Seeding is driven by the same
  PCG stream and produces the same world contents on both backends.
- *Delivery semantics*: still at-least-once with idempotent handlers. Nothing
  in this ADR claims exactly-once.

## Consequences

The connection pool is bounded (16) rather than unlimited, so a crash-looping
worker cannot exhaust server connections. A `search_path=<schema>` parameter
scopes all tables to one schema and creates it if absent, which is how parallel
integration tests and separate environments share a server; the schema name is
validated as a plain identifier before it reaches `CREATE SCHEMA`, since it
cannot be a bind parameter.

PostgreSQL tests are opt-in behind `TWINWRIGHT_TEST_POSTGRES_DSN` and skip when
it is unset, so `go test ./...` stays hermetic. They run against a real server,
never a mock: a mock cannot reproduce the integer width, grouping and type
strictness differences that are the entire reason these tests exist.
