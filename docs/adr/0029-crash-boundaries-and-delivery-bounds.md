# 0029. Crash boundaries, delivery bounds, and what is idempotent

Date: 2026-10-26

Status: accepted; amends ADR 0020

## Context

ADR 0020 made recovery from a dead worker a headline property and described how
it was tested. Reading the tests against the sentences showed the sentences went
further than the tests did, in three places (listed under Corrections below). The
code was also missing four things the claim needed: a bound on how often a run
that kills its worker is redelivered, an ordering of lease and queue times that
SQL could trust, a way to crash at any durable boundary rather than only inside a
provider call, and a fence on the commands that run an agent locally.

This ADR records what was built to close those gaps, states precisely what is
idempotent and under which conditions, and corrects ADR 0020 where it overstated.
ADR 0020's body is left as written, with a pointer to this record, because it is
the record of what was believed then.

## Decision

### The seven boundaries

`internal/crashpoint` names the points at which a process can die. `agent.Runner`
and `dispatch.Dispatcher` each carry a `Crash` hook that is nil in production. A
hook that returns an error is a simulated death: the caller returns at once and
writes nothing further, not even an error event or a failed status.

| Point | Durable state at the moment of death |
| --- | --- |
| `before-model-request` | previous step fully committed; no request for this turn |
| `after-model-request` | `model.request` committed; no response |
| `after-model-response` | turn committed (response, step, transcript); its tool calls not dispatched |
| `before-tool-commit` | inside the tool transaction with every write staged, not committed |
| `after-tool-commit` | tool transaction committed (effect, events, saved result, transcript) |
| `before-completion` | final answer committed; run not marked completed |
| `after-completion` | run completed; queue entry not finished |

`after-completion` is never fired by code, because nothing runs after the
completed status is written. A harness simulates it by skipping `FinishRun`.
That is a model of the death, not an observation of one.

### Proof: an exhaustive matrix and real process death

`internal/crashmatrix` runs a scenario once without a crash to count every
occurrence of every point. For each (point, occurrence) it crashes a first
delivery there, expires the lease, lets a second worker take over, and checks:

- the run completes under a strictly higher fence and is reported as a takeover;
- the number of committed agent mutations equals the uncrashed run's, and no call
  ID committed two;
- per-table world row counts equal the uncrashed run's;
- `replay.Verify` passes on the recovered ledger;
- the latest checkpoint at or before the crash, and the final one, reconstruct;
- model requests equal model responses;
- the dead worker's fence is refused for a new mutating call and for a call ID
  that was already committed.

`TestCrashMatrixSQLite` runs every (point, occurrence) of three scenarios:
`duplicate-charge` (25 cases), `ambiguous-commit` (15) and `company-incident`
(80), 120 in all, with zero violations. `TestCrashMatrixPostgres` runs the first
and last occurrence of each point when `TWINWRIGHT_TEST_POSTGRES_DSN` is set. It
skips on a host without a server, and no PostgreSQL run had happened when this
was written. It runs in the Integration workflow, and until that has run green
the PostgreSQL half of the claim is unproven.

The matrix cannot show what survives a process that dies holding an open
transaction, because its "death" is a cancelled context in a process that is
still alive and whose connections close in an orderly way.
`TestRealProcessDeathIsRecovered` (unix only) re-executes the test binary as a
worker that sends itself `SIGKILL`, then recovers with a second worker against
the same database file. It covers four crashes:

| Crash | Durable before recovery |
| --- | --- |
| `after-model-request`, occurrence 2 | not asserted |
| `after-model-response`, occurrence 3 | not asserted |
| `before-tool-commit`, `createRefund`, occurrence 1 | no refund |
| `after-tool-commit`, `createRefund`, occurrence 1 | one refund |

The kill is issued before the run context is cancelled, by construction in
`CrashWith`. The other order lets
`database/sql` observe the cancellation and roll the open transaction back
cleanly, which would turn a test of a dead client into a test of an orderly one.
`TestRealProcessDeathIsRecoveredPostgres` covers the two tool-commit rows against
a server when a DSN is set. The other three points (`before-model-request`,
`before-completion`, `after-completion`) are covered by the in-process matrix
only.

### A call ID cannot be reused within a run

A provider turn that reuses a call ID already used in the run, or repeats one
within the turn, fails with an explicit error instead of being skipped.
`TestProviderCallIDReuseFailsTheTurn` in `internal/agent/callid_test.go` holds
this for both shapes: a reuse across turns (the run fails with `reused call ID`)
and a repeat inside one turn. Idempotency is keyed on (run, call ID), so an ID
that means two requests makes the guarantee ambiguous, and skipping the second
one hid that from the ledger.

### Coordination timestamps sort as text

The lease expiry and the queue availability time are compared inside SQL as text.
`time.RFC3339Nano` trims trailing zeros, so `12:00:01Z` sorted after
`12:00:01.5Z`, and within one second a renewal could revive an expired lease while
a claim could miss an expired lease or a run that had become available.

Every time written to `run_leases`, `work_queue` and `workers` now uses
`store.CoordinationLayout`, `2006-01-02T15:04:05.000000000Z`, in UTC. It is still
RFC 3339, so existing parsers read it, and lexical order equals time order.
Ledger `events` timestamps are not coordination times and are unchanged, because
replay compares them byte for byte.

Migration 3, "canonical coordination timestamps" (`store.CurrentSchemaVersion` is
now 3), rewrites existing rows into the new form. It leaves an empty value alone,
since that means "not set". It refuses any other value that does not parse and
names the table, column and row key, and the failure rolls the migration back
(`TestMigrationThreeNormalizesCoordinationTimestamps`,
`TestMigrationThreeRefusesAnUnparseableTimestamp`,
`TestMigrationThreeLeavesEmptyTimestampsAlone`).

Upgrade caveat: stop every worker running an older build before applying
migration 3. An older build keeps writing the variable-width form, and a database
holding both forms misorders times within a second until the old rows are
rewritten. The migration only rewrites what exists when it runs.

### A delivery bound that a crash cannot dodge

`MaxAttempts` was checked only when execution returned an error. A worker that
dies returns nothing, so a run that killed its worker every time was taken over
forever. The claim count is the one signal a crash leaves behind, so a delivery
that never reports back now counts toward the bound.

When a claim's attempts exceed `MaxAttempts` (default 3, so the fourth delivery):

- the run is not executed;
- if the run is already `completed` (it died at `after-completion`), the worker
  finishes the queue entry normally;
- otherwise, under its fresh lease and in one fenced transaction, it sets the run
  to `failed`, appends an `error` ledger event
  `{"kind":"delivery_limit","deliveries":N,"max_attempts":M}`, and marks the queue
  entry failed.

The payload carries counts only. The ledger records how execution ended, not who
owned it. `Outcome.Takeover` was `attempts > 1`, so an ordinary requeue after an
error counted as a takeover; it is now true only when the claim displaced an
expired lease. `store.Enqueue` resets the attempt count to zero, so an explicit
operator retry of a run that failed at the limit gets a fresh budget instead of
being refused again. Tests are in `internal/worker/delivery_test.go`:
`TestRunThatKillsEveryDeliveryIsFailedAtTheDeliveryLimit`,
`TestCompletedRunPastTheDeliveryLimitIsFinishedNotFailed`,
`TestReenqueuedFailedRunGetsAFreshDeliveryBudget`,
`TestRequeueAfterAnErrorIsNotReportedAsATakeover` and
`TestExpiredLeaseClaimIsReportedAsATakeover`.

### Local execution holds the same lease

`twinwright run` without `--enqueue`, and `twinwright resume`, used to execute
with no fence, so they could write to a run at the same moment as a fleet worker
that had claimed it. They now go through `worker.ExecuteLocal`, which acquires a
lease owned by `local-<host>-<pid>`, with a 15 second TTL, renews it while the
command executes, passes its fence to the runner and dispatcher, and releases it
on exit, including on Ctrl-C and `SIGTERM`.

A run leased by a live fleet worker is refused with `store.ErrLeaseHeld`, which
names the owner and the expiry. A fleet worker cannot claim a run while a local
lease is live, because `ClaimRun` already excludes runs under another owner's
unexpired lease. After `kill -9` of a local command the lease stays until it
expires, up to 15 seconds, so an immediate `resume` is refused until then. A
release that fails is reported rather than swallowed; on a run that completed it
is `worker.ErrLeaseNotReleased`, and the run itself is not reported as failed.
Tests are in `internal/worker/local_test.go`, beginning with
`TestExecuteLocalFencesItsWrites`,
`TestExecuteLocalRefusesARunLeasedByAFleetWorker` and
`TestFleetCannotClaimARunExecutingLocally`.

### The bench uses the harness

`bench` distributed mode runs through `crashmatrix`. Case fields `crash_point`
and `crash_operation` select a boundary and an operation
(`crash_operation` only with `before-tool-commit` or `after-tool-commit`;
`after-completion` only with `crash_at` 1). An integer `crash_at` alone keeps its
meaning of `after-model-request` at that turn. The standard suite is now 21 cases,
including `distributed-crash-after-refund-commit`, and
`TestFlagshipSuiteRecoversEveryCrashCase` runs `flagship.yaml` inside `go test`.

## What is idempotent, and under which conditions

- A world mutation made by a tool call commits at most once per (run, call ID).
  The effect, its ledger events, its saved result and its transcript entry commit
  in one transaction that first proves lease ownership.
- Model calls are at-least-once. A crash before a response is recorded repeats
  the provider call, and its cost.
- A retry with a new call ID is a new logical action and is not deduplicated.
  That is the behaviour the `ambiguous-commit` scenario measures.
- Effects outside the world store (provider spend, logs, metrics) are not fenced.

Delivery remains at-least-once and exactly-once is not claimed.

## Corrections to ADR 0020

ADR 0020 overstated three things.

**"Crash recovery at every model turn" was crash recovery inside a provider
call.** `TestRecoveryFromCrashAtEachModelTurn` cancels the run from inside the
provider on turn N. That is the `after-model-request` boundary, and only that
one. It never crashed between a turn's commit and its tool calls, inside or just
after a tool transaction, or around completion. The exhaustive coverage that the
sentence implied is `TestCrashMatrixSQLite`, added now, and the real-process
check is `TestRealProcessDeathIsRecovered`.

**The call-ID cache was not what protects a recovered run.** ADR 0020 called
idempotency by call ID "load-bearing" and "the mechanism that makes at-least-once
delivery safe". During recovery the resumed runner dispatches only calls that have
no tool message in the transcript (`pending` in `internal/agent/runner.go`), and
that transcript entry commits in the same transaction as the effect. A committed
call therefore is not presented to the dispatcher again. The saved-result lookup
still answers a repeated (run, call ID) pair, and
`TestDuplicateDeliveryDoesNotDuplicateCommittedEffects` shows a redelivered
finished run committing nothing new, but the crash tests do not depend on the
cache to avoid a duplicate. Atomic commit of effect and transcript, plus the
fence, carry that load.

**The lease unit tests did not run against PostgreSQL.** ADR 0020 said each test
"runs against SQLite by default and against real PostgreSQL when a DSN is
configured". The worker tests in `internal/worker/worker_test.go` choose their
store through `pgtest`. The lease unit tests in `internal/store/lease_test.go`
and `internal/store/lease_subsecond_test.go` open SQLite and have no PostgreSQL
variant. PostgreSQL exercises the fence through the worker tests and, from this
ADR on, `TestCrashMatrixPostgres`, which cannot be said to have run until the
Integration workflow has run it.

## Remaining limits

- `fork`, `counterfactual` and the non-distributed `bench` modes execute their
  own new runs without a fence. So does the shadow simulation. `bench`
  distributed mode runs through the fenced `crashmatrix` harness. Each works on a run it created, but none
  holds a lease while it does.
- The real-process test kills one process at a time. It does not show concurrent
  multi-process contention on one SQLite file.
- The matrix covers three scenarios. A fourth world or a different tool shape is
  not covered until a scenario is added.
- Detection of a crash still depends on a lease expiring. A dead worker's run is
  not recovered until its lease lapses, up to the worker TTL, and up to 15
  seconds after a killed local command.
- `after-completion` is simulated by skipping `FinishRun`, not observed from a
  dying process.
- No PostgreSQL run of the matrix or the PostgreSQL real-process test had
  happened when this was written.

## Consequences

The published guarantee is now narrower than ADR 0020's prose and no wider
than the tests. The cost is an upgrade step, stopping old workers before migration
3, and a run that kills its worker three times now ends `failed` with a ledger
event saying why, where before it was retried indefinitely. An operator who
believes the cause was transient re-enqueues it and gets a fresh budget.
