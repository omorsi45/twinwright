# ADR 0020: Multi-worker runtime, leases in the world store, and fencing

Status: accepted, 2026-09-26; supersedes ADR 0018, completes ADR 0017

ADR 0017 refused to call lease primitives a distributed runtime and listed what
had to exist first: consistency, delivery semantics, idempotency, transaction
boundaries, lease ownership, worker recovery, duplicate-delivery handling and
event ordering. ADR 0018 added a standalone SQLite lease store as a first step.
ADR 0019 added PostgreSQL storage. This ADR adds the execution half and states
the semantics.

## The defect in ADR 0018, and the correction

ADR 0018 put leases in a database file *separate* from the world store. That
makes fencing unenforceable. A fence is only meaningful if the ownership check
and the write it protects commit atomically; with two databases there is no
transaction spanning both, so a worker could pass the check, stall, lose the
lease, and still commit. The check would be decoration.

Run ownership therefore moved into the world store: `run_leases` lives beside
`runs` and `events`, and the fence check runs *inside the caller's
transaction*. The standalone lease package and its `twinwright lease` command
are removed rather than left in place, because two lease implementations with
different guarantees is a trap for whoever reads the code next.

## Architecture

- `work_queue` holds runs awaiting execution, with a max-step budget, an
  attempt counter and an availability time for backoff.
- `run_leases` holds one row per run: current owner, current fence, the highest
  fence that has committed, and expiry.
- `run_ownership_log` is an append-only audit of ownership transitions.
- `workers` records the fleet for operators.
- `twinwright run --enqueue` creates a run and queues it. `twinwright worker`
  claims, executes and finishes runs. `twinwright queue` reports depth, fleet
  and per-run ownership.

A worker claims the oldest run that is runnable, *or* marked leased with an
expired lease - which is exactly what a killed process leaves behind. Recovery
needs no janitor process: the next poll picks the run up. On PostgreSQL the
candidate row is taken with `FOR UPDATE OF q SKIP LOCKED`, so simultaneous
pollers take different runs instead of contending. SQLite has a single writer,
so the transaction alone provides that exclusion.

## Delivery semantics: at-least-once, and why duplicates are safe

Delivery is **at-least-once**. Exactly-once delivery is not claimed here or
anywhere in the codebase, because it is not achievable across a process
boundary and a database without making the effect itself idempotent - which is
what Twinwright does instead.

A duplicate delivery does not duplicate committed tool effects, for two
independent reasons:

1. **Idempotency by call ID.** The dispatcher looks up `(run_id, call_id)`
   before executing. A repeated call returns the recorded status and body and
   commits nothing. The same call ID with *different* arguments is refused
   rather than answered from the cache, so a cache hit can never stand in for
   a different request.
2. **Fencing.** Every durable write passes a check that the presented fence
   still owns the run, inside the same transaction as the write. A worker that
   stalled past its lease cannot commit after a newer worker took over.

The fence check has two conditions, and both are needed:

- The lease row must still name this owner and token and must not have expired.
  Expiry alone disqualifies, because the instant a lease lapses another worker
  may take the run.
- The presented token must be at least `committed_fence`, the highest token
  that has already committed. This condition involves no clock at all, so it
  stays correct under clock skew between workers and the database - precisely
  the case a TTL check alone gets wrong.

Every acquisition increments the fence, *including* re-acquisition by the same
owner. A worker that lost contact and reconnected must not keep using its old
token, because the run may have been taken over and handed back in between.

While a run executes, the worker renews its lease on a timer. If renewal is
rejected the execution context is cancelled immediately: continuing would spend
model calls on work this worker is no longer permitted to commit.

## Ownership history is not in the run ledger

Ownership transitions are recorded in `run_ownership_log`, not as ledger events.

This is a deliberate boundary. Fork lineage and checkpoint reconstruction digest
a run's event prefix. If ownership transitions lived there, the prefix digest
would depend on which worker executed the run and how many times the lease
changed hands, so an identical agent trajectory would produce different digests
on a single node and in a fleet, and replay would have to special-case events it
cannot regenerate. Keeping the ledger a pure record of what the *agent* did
means a distributed run and a single-node replay of the same trajectory produce
byte-identical ledgers - which the crash-recovery tests assert by replaying
every recovered run.

One consequence, stated plainly: a fencing rejection cannot be a ledger event
either, because the rejected worker has no authority to append to that run. It
is recorded in the ownership audit log and in worker logs.

## A durability bug this work found and fixed

A process that died between persisting `model.request` and recording
`model.response` left the ledger ending in an unanswered request. On resume the
runner appended a *second* request, so the ledger held more requests than
responses permanently, and replay - which pairs them - rejected the run forever.
A crash therefore made a run unauditable for the rest of its life.

The fix: the request payload derives only from the task, provider, model,
transcript and exposed operations, so a resumed attempt regenerates it byte for
byte. The runner now recognises a trailing unanswered request as *this*
attempt's request and reuses it. A trailing request whose payload differs is not
a resumed attempt - something changed underneath the run - and that fails loudly
rather than silently attributing one request to a different context.

Removing this reuse makes `TestRecoveryFromCrashAtEachModelTurn/crash_on_model_turn_2`
fail, which was verified by deliberately disabling it.

## ADR 0017's guarantees, mapped

- *World isolation*: unchanged; queries are still scoped by `world_id`.
- *Deterministic initialization*: unchanged; seeding does not involve workers.
- *Atomic local mutation with ledger effects*: unchanged, and now additionally
  gated on ownership. The dispatcher's single transaction proves the fence
  before the idempotency lookup, so a fenced-out worker cannot even read the
  run's saved results, let alone add to them.
- *Tool call idempotency*: unchanged and load-bearing, now the mechanism that
  makes at-least-once delivery safe rather than merely tolerable.
- *Durable pause/resume*: unchanged, plus the model-request fix above.
- *Source-safe replay*: unchanged. Replay reconstructs into a throwaway local
  SQLite database, so verifying a run cannot mutate a shared runtime.
- *Explicit authorization*: unchanged; authorization still runs inside the tool
  transaction, before any handler.
- *Event ordering*: unchanged. Sequence numbers are allocated inside the
  transaction that appends the event, and a run has one owner at a time, so
  two workers cannot interleave appends on one run.
- *Honest capability boundaries*: at-least-once delivery is stated in the
  package documentation, in the CLI's JSON output, and here.

## What this is not

This is a multi-worker runtime over one database. It is not a multi-region
system, it has no message broker, and it does not shard. A broker was rejected:
the queue's requirements are claim-one-row-atomically and take-over-on-expiry,
which PostgreSQL does with `SKIP LOCKED` in the same transaction as the lease -
adding Redis or a queue service would introduce a second source of truth to keep
consistent with the ledger for no capability gained. Kubernetes was likewise
rejected: a worker is a process that needs a DSN, so how it is scheduled is an
operational choice, not an architectural one.

## Testing

The claims above are asserted, not asserted-in-prose. Tests cover: fence
rejection after takeover; rejection of an expired lease with no competitor;
rejection of a superseded-but-unexpired token (the clock-skew case); a crashed
worker taken over with the run completing exactly once; crash recovery at every
model turn of a real trajectory, each verified by replaying the recovered run;
an interrupted tool transaction leaving no mutation, event or saved result; a
worker abandoning execution mid-flight when it loses its lease; several
independent runs progressing in parallel with no re-delivery; and six workers
racing for one run with exactly one winner. Each runs against SQLite by default
and against real PostgreSQL when a DSN is configured.
