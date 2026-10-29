# The distributed runtime

This page was moved out of the project README when the README was rewritten as the Twinwright Cloud overview. It holds the README section "Distributed runtime", unchanged except where noted. The description of what `docker compose up -d` starts was corrected and its comment in the first code block now names all four services, and a paragraph on `make up` and `make down` was added. A duplicate delivery is safe for three reasons, not two (atomic commit was added to idempotency and fencing), and ADR 0029 was added to the references. The sentence on the fence check was corrected: the lease row must still name the presented owner and token, which needs no clock, and the highest committed fence is a record on top of it, not the check that is immune to skew. Four things were added to moved paragraphs: that PostgreSQL's host port is `TW_DEMO_PG_PORT`, that a renewal that merely fails is retried while the lease is valid, that `doctor` and `queue` open the database read-only and never migrate it, and that the timestamps stored as text are the ones replay compares in the world tables. Paths in backticks are relative to the repository root. [Back to the guide index](README.md).

## Distributed runtime

Twinwright runs on one process with SQLite by default. For a fleet, point it at
PostgreSQL and run workers.

```bash
docker compose up -d                    # PostgreSQL, the collector, Jaeger and Prometheus, all on 127.0.0.1
export TW_DSN='postgres://twinwright:twinwright@127.0.0.1:5432/twinwright?sslmode=disable'

go run ./cmd/twinwright build examples/billing/openapi.yaml \
  --bindings examples/billing/bindings.yaml --out twinwright.manifest.json

# Create a run and queue it instead of executing it here.
go run ./cmd/twinwright run duplicate-charge --db "$TW_DSN" --agent scripted --enqueue --steps 10

# Run one or more workers. Each claims queued runs under a fenced lease.
go run ./cmd/twinwright worker --db "$TW_DSN" --lease-ttl 30s --metrics-addr 127.0.0.1:9095

go run ./cmd/twinwright queue --db "$TW_DSN"                 # depth and fleet
go run ./cmd/twinwright queue --db "$TW_DSN" --run <run-id>   # ownership history
```

`docker compose up -d` starts four services, each published on loopback only: PostgreSQL (5432),
the OpenTelemetry collector (4318), Jaeger (16686) and Prometheus (9090). PostgreSQL's host port
is `${TW_DEMO_PG_PORT:-5432}`: set `TW_DEMO_PG_PORT` before `docker compose up` if something else
holds 5432, and `make demo-stack` reads the same variable, so it connects to the port Compose
published. The DSN above, `POSTGRES_DSN` in the Makefile and the README examples name 5432 and
need the new port too. Name `postgres`
(`docker compose up -d postgres`) to start the database alone. `make up` starts the same four and
waits for PostgreSQL; `make down` runs `docker compose down -v`, which deletes the Compose volumes,
including the `twinwright` database in that container.

`--drain` exits when the queue empties, which is what CI uses. The same database
also serves `replay`, `evaluate`, `trace` and `inspect`, so a distributed run is
as auditable as a local one.

### Semantics

Delivery is **at-least-once**. A crashed worker's run is taken over and the same
work is attempted again. Exactly-once is not claimed anywhere, because it is not
achievable across a process boundary and a database. A duplicate delivery does
not duplicate a committed world mutation, for these reasons:

- **Atomic commit.** A tool call's effect, its ledger events, its saved result and
  its transcript entry commit in one transaction. A resumed run dispatches only
  the calls its transcript does not yet record, so a committed call is not
  presented again.
- **Idempotency by call ID.** A repeated tool call returns the result recorded the
  first time and commits nothing. The same call ID with different arguments is
  refused rather than answered from the cache, and a provider turn that reuses a
  call ID already used in the run fails the turn.
- **Fencing.** Every durable write proves, inside the same transaction as the
  write, that this worker still owns the run. A worker that stalled past its lease
  cannot commit after a newer worker took over.

What that does and does not cover: a world mutation commits at most once per
(run, call ID). Model calls are at-least-once, so a crash before a response is
recorded repeats the provider call and its cost. A retry with a new call ID is a
new logical action and is not deduplicated, which is what the `ambiguous-commit`
scenario measures. Provider spend, logs and metrics are not fenced.

**Crash boundaries.** A run can die at seven named points: `before-model-request`,
`after-model-request`, `after-model-response`, `before-tool-commit`,
`after-tool-commit`, `before-completion` and `after-completion`.
`TestCrashMatrixSQLite` crashes every occurrence of every point across three
scenarios (120 cases) and checks that a second worker finishes the run with no
duplicated mutation, a ledger that still replays, checkpoints that still
reconstruct, and a stale worker that cannot commit. `TestCrashMatrixPostgres` runs
the first and last occurrence of each point when `TWINWRIGHT_TEST_POSTGRES_DSN` is
set, in the Integration workflow. `TestRealProcessDeathIsRecovered` (unix) kills a
real worker process with `SIGKILL` at four of those points. The table of what is
durable at each point, and which test proves it, is in
[docs/distributed.md](../distributed.md#crash-boundaries).

**Delivery bound.** A delivery that never reports back counts toward the worker's
`MaxAttempts` (default 3). A run claimed after that is not executed: if it had
already completed its queue entry is finished, and otherwise it is marked `failed`
with an `error` ledger event `{"kind":"delivery_limit","deliveries":N,"max_attempts":M}`.
Re-enqueueing the run gives it a fresh budget. A run retried this way can
complete but cannot be verified by replay, because its ledger records the
`delivery_limit` error and an unanswered model request.

**Local execution.** `run` without `--enqueue`, and `resume`, hold a lease owned by
`local-<host>-<pid>` with a 15 second TTL, renewed while they run and released on
exit, including on Ctrl-C. A run leased by a live fleet worker is refused, naming
the owner and expiry. After `kill -9` the lease has to expire, up to 15 seconds,
before `resume` is accepted.

Every lease acquisition raises the fence, including re-acquisition by the same
owner, so a reconnected worker cannot reuse an old token. The check rejects a token
that is not the one the lease row names - a match that needs no clock and is therefore
immune to clock skew - and an expired lease. The highest fence that has committed is
recorded as well, and refuses a lower one even if the row's owner and fence were put
back by hand. Losing a renewal cancels execution
immediately rather than spending model calls on work that can no longer commit.
A renewal that merely fails (a dropped connection, a locked SQLite file) is
retried at the next interval while the worker's own clock says the lease is still
valid, and the run is abandoned only once it is not.

A run is claimable when it is runnable, or marked leased with an expired lease,
which is what a killed process leaves behind, so recovery needs no janitor. On
PostgreSQL the candidate row is taken with `FOR UPDATE ... SKIP LOCKED`, so
simultaneous pollers take different runs.

Ownership history is recorded in `run_ownership_log`, **not** in the run ledger.
Keeping the ledger purely about the agent is what lets every
recovered run still replay, which the crash-recovery tests assert by replaying each
one.

See `docs/adr/0019-postgres-storage.md`,
`docs/adr/0020-multi-worker-runtime.md` and
`docs/adr/0029-crash-boundaries-and-delivery-bounds.md`.

### Storage

```bash
go run ./cmd/twinwright doctor --db "$TW_DSN"   # backend, schema version, queue depth
go run ./cmd/twinwright version                  # includes the schema version this build writes
```

`doctor` and `queue` open the database read-only. `doctor` reports the schema
version the database has now, and when that is behind this build it says which
version a migration would reach and that it has not applied it. Neither command
migrates the database or creates one for a mistyped `--db`, so they are safe to
run while workers on an older build are still up.

Schema state is versioned in `schema_migrations`. Each migration commits with its
own bookkeeping row, so a failed migration leaves neither a partial schema nor a
false record of success. A database written before migration tracking is adopted
in place; a database recording a newer version is refused rather than downgraded.
Timestamps are stored as RFC 3339 text on both backends so the `created_at` values
that replay compares in the world tables are the same text on both. Replay does not
compare a ledger event's `recorded_at`. Times in `run_leases`, `work_queue` and `workers` use the
fixed-width form `2006-01-02T15:04:05.000000000Z` so that comparing them as text
in SQL orders them as times; migration 3 (schema version 3) rewrites existing
rows and refuses a value it cannot parse. **Upgrading:** stop workers running an
older build before applying it, because an older build keeps writing the
variable-width form and mixed forms misorder within a second.
