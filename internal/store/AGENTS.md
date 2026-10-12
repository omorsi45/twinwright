# Persistence and schema instructions

Applies to `internal/store/` in addition to its ancestors' instructions.

- SQLite is the default local backend; PostgreSQL parity is required for
  persistent and distributed behavior. Use the package's dialect and SQL
  placeholder rewriting instead of assuming one database's syntax.
- All statements with external values must use bind parameters. Validate
  schema identifiers before interpolation.
- Schema changes require a versioned migration and tests for upgrade,
  repeated open, and refusal of newer unsupported schemas. Never silently
  rewrite existing event history.
- Guard ownership-sensitive writes with fencing in the same transaction
  as the data change. Keep lease/ownership bookkeeping out of the event
  ledger consumed by replay and checkpoints.
- Assert atomic changes, call-ID behavior, and read-only opens. Test both
  backends when a query or migration changes.

From the root:
```sh
go test -count=1 ./internal/store
make test-postgres   # only when PostgreSQL is running
```

Consult `docs/adr/0018-lease-fencing.md`,
`docs/adr/0019-postgres-storage.md`, and
`docs/adr/0020-multi-worker-runtime.md` before changing persistence
or lease semantics.
