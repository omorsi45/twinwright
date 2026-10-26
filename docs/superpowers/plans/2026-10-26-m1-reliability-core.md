# Milestone 1: Reliability Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A worker crash at any documented execution boundary is recovered by another worker with no duplicated world mutation, a ledger that still replays, checkpoints that still reconstruct, and a stale worker that cannot commit, proven in-process and by real process death.

**Architecture:** Fix the two verified recovery defects (text-ordered lease timestamps, unbounded crash redelivery), add an `internal/crashpoint` hook at named boundaries in the runner and dispatcher, build a reusable `internal/crashmatrix` harness that enumerates and verifies every (boundary, occurrence), back it with a real `SIGKILL` test, and route local execution and the bench's distributed mode through the same fenced paths.

**Tech Stack:** Go 1.27.2, SQLite (modernc), PostgreSQL (pgx) behind `TWINWRIGHT_TEST_POSTGRES_DSN`, standard library only.

**Spec:** `docs/superpowers/specs/2026-10-26-twinwright-cloud-design.md` (Milestone 1 section is binding).

## Global Constraints

- Work only in `/Users/omarmorsi/Desktop/Github/twinwright/.worktrees/cloud-m1-reliability-core` on branch `cloud/m1-reliability-core`.
- Every shell starts with `export PATH="/usr/bin:/opt/homebrew/bin:$PATH"` (`/usr/local/bin/git` is a broken x86 binary on this host).
- Go 1.27.2. No new module dependencies: `go.mod` and `go.sum` stay byte-identical, `go mod tidy` produces no diff.
- `go test ./...` stays hermetic: no network, API key, Docker or PostgreSQL. PostgreSQL tests skip unless `TWINWRIGHT_TEST_POSTGRES_DSN` is set and use `internal/pgtest` (`pgtest.Available()`, `pgtest.SchemaDSN(t)`). Real-process tests carry `//go:build unix`.
- Each commit passes `make check` and `go test -count=1 ./...`. The last task also passes `go test -race -count=1 ./...`, `./scripts/e2e.sh` and `make bench-smoke`.
- Preserve the `AGENTS.md` invariants: `world_id` isolation; call-ID idempotency with different-argument reuse refused; local effects and their ledger events commit atomically; authorization inside the tool transaction before handlers; replay and inspection never mutate the source; fenced durable writes in the same transaction; at-least-once delivery, never described as exactly-once.
- The ledger (`events`) never records worker identity, owner names, fences or lease data.
- Canonical coordination timestamp layout, exactly: `2006-01-02T15:04:05.000000000Z` (UTC, nine fractional digits). Applies to `run_leases`, `work_queue`, `workers`. Ledger `events` timestamps are not changed.
- Crash point names, exactly: `before-model-request`, `after-model-request`, `after-model-response`, `before-tool-commit`, `after-tool-commit`, `before-completion`, `after-completion`.
- Delivery-limit ledger event: type `error`, payload exactly `{"kind":"delivery_limit","deliveries":N,"max_attempts":M}`.
- Local execution lease owner: `local-<hostname>-<pid>`.
- Match the repository's style: `gofmt`, doc comments that explain why, table-driven tests, existing helper patterns.
- Written text (comments, docs, commit messages) uses no em dashes, claims only what a test proves, and carries no assistant attribution or `Co-authored-by` trailer. Commit messages explain why.
- Never weaken an invariant or a test to get green. If the crash matrix exposes a production bug, fix it inside the crash-recovery path with a regression test, or report BLOCKED with the failing cases.

## Review Focus

- Upgrading a v0.1.0 database (schema 2) that holds queued runs and live leases written in variable-width RFC 3339: after migration 3, claims and renewals must follow time order. Pinned by `TestMigrationThreeNormalizesCoordinationTimestamps` (Task 1), SQLite and PostgreSQL.
- A provider, such as a local OpenAI-compatible server, that emits the same call ID in two turns: the run must fail with an error naming the ID, never skip the call. Pinned by `TestProviderReusingACallIDAcrossTurnsFailsTheTurn` (Task 6).
- Ctrl-C during a local `run`: the lease must be released so an immediate `resume` works; after `kill -9` the refusal must name the owner and the expiry. Pinned by `TestExecuteLocalReleasesTheLeaseWhenCancelled` and `TestExecuteLocalRefusesARunLeasedByAFleetWorker` (Task 7).
- A provider call slower than the lease TTL during local execution: the lease must be renewed, not lost. Pinned by `TestExecuteLocalRenewsItsLease` (Task 7).
- Bench suites written for v0.1.0 that use only an integer `crash_at`: they must parse and measure exactly as before. Pinned by `TestIntegerCrashAtKeepsItsMeaning` (Task 8).

## Roadmap

Each milestone is its own stacked branch, plan, review and finish. Later plans are written against the code as it stands when they start.

| # | Milestone | Branch | Plan |
|---|---|---|---|
| 1 | Reliability core | `cloud/m1-reliability-core` | this file |
| 2 | Regression gate and reproducible reports | `cloud/m2-regression-gate` | written after M1 finishes |
| 3 | Real provider execution, recording, offline reconstruction | `cloud/m3-real-providers` | after M2 |
| 4 | Observability against a real collector and Prometheus | `cloud/m4-observability` | after M3 |
| 5 | Bring-your-own-agent protocol and isolation boundary | `cloud/m5-agent-protocol` | after M4 |
| 6 | Reproducible reliability and performance benchmarks | `cloud/m6-benchmarks` | after M5 |
| 7 | Twinwright Cloud packaging and documentation | `cloud/m7-packaging` | after M6 |

## File Structure (Milestone 1)

| File | Responsibility |
|---|---|
| `internal/store/timestamp.go` (new) | canonical coordination timestamp formatter |
| `internal/store/queue.go`, `lease.go` | write canonical stamps; `QueueEntry.Takeover`; `FailRunAtDeliveryLimit` |
| `internal/store/migrate.go` | migration `rewrite` hook; migration 3 |
| `internal/worker/worker.go` | delivery bound; honest takeover flag |
| `internal/worker/local.go` (new) | `ExecuteLocal`: fenced, renewed, released local execution |
| `internal/crashpoint/crashpoint.go` (new) | boundary names, `Site`, `Hook`, `Fire`, `ErrCrashed` |
| `internal/agent/runner.go` | fire runner boundaries; refuse call-ID reuse |
| `internal/dispatch/dispatch.go` | fire tool-commit boundaries |
| `internal/crashmatrix/*.go` (new) | scenarios, enumeration, crash, recovery, invariant verification |
| `internal/bench/suite.go`, `modes.go` | `crash_point`, `crash_operation`; distributed mode on `crashmatrix` |
| `cmd/twinwright/main.go` | `run` and `resume` through `ExecuteLocal` with signal handling |
| `examples/bench/standard.yaml` | post-refund crash case; corrected comment |
| `docs/adr/0029-crash-boundaries-and-delivery-bounds.md` (new), `docs/distributed.md`, `README.md`, `CHANGELOG.md` | the published contract |

---

### Task 1: Canonical coordination timestamps and migration 3

**Files:**
- Create: `internal/store/timestamp.go`
- Modify: `internal/store/queue.go`, `internal/store/lease.go` (every value written to `run_leases`, `work_queue`, `workers`; find them with `grep -n "Format(time.RFC3339Nano)" internal/store/queue.go internal/store/lease.go`)
- Modify: `internal/store/migrate.go` (migration `rewrite` hook, migration 3, `CurrentSchemaVersion = 3`)
- Test: `internal/store/lease_subsecond_test.go`, `internal/store/timestamp_test.go`, PostgreSQL variant in `internal/store/postgres_test.go`

**Interfaces:**
- Produces: `const CoordinationLayout = "2006-01-02T15:04:05.000000000Z"`; `func CoordinationTime(t time.Time) string` (formats `t.UTC()` with that layout); `migration.rewrite func(ctx context.Context, tx *sql.Tx, d Dialect) error`, run by `applyOne` after statements and columns, inside the same transaction.

- [ ] **Step 1: Write the behavioural tests against current code** in `lease_subsecond_test.go`, using `leaseTestStore` and `seededRun` from `lease_test.go`:

```go
base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
// TestRenewRejectsALeaseThatExpiredEarlierInTheSameSecond
lease, _ := s.AcquireRunLease(ctx, run.ID, "worker-a", time.Second, base) // expires 12:00:01.000
_, err := s.RenewRunLease(ctx, lease, time.Second, base.Add(1500*time.Millisecond))
if !errors.Is(err, ErrFenced) { t.Fatalf("renewed an expired lease: err=%v", err) }

// TestClaimTakesOverALeaseThatExpiredEarlierInTheSameSecond
_ = s.Enqueue(ctx, run.ID, 10, base)
first, _, _ := s.ClaimRun(ctx, "worker-a", time.Second, base)
second, _, err := s.ClaimRun(ctx, "worker-b", time.Second, base.Add(1500*time.Millisecond))
// want err == nil, second.Owner == "worker-b", second.Token == first.Token+1

// TestRequeuedRunIsClaimableWithinTheSecondItBecomesAvailable
lease, _, _ := s.ClaimRun(ctx, "worker-a", time.Minute, base)
_ = s.RequeueRun(ctx, lease, base, base.Add(time.Second), "retry") // available 12:00:01.000
_, entry, err := s.ClaimRun(ctx, "worker-b", time.Minute, base.Add(1500*time.Millisecond))
// want err == nil, entry.RunID == run.ID
```

- [ ] **Step 2: Run them and confirm they fail for the right reason**

Run: `go test -count=1 -run 'SameSecond|WithinTheSecond' ./internal/store/`
Expected: FAIL. Renew succeeds, and both claims return `ErrNoWork`.

- [ ] **Step 3: Write the format and migration tests** in `timestamp_test.go`:

```go
// TestCoordinationTimeIsFixedWidthAndOrdered
a := time.Date(2026, 1, 1, 12, 0, 1, 0, time.UTC)
want "2026-01-01T12:00:01.000000000Z" == CoordinationTime(a)
want CoordinationTime(a) < CoordinationTime(a.Add(500*time.Millisecond))
want CoordinationTime(a.In(time.FixedZone("EST", -5*3600))) == CoordinationTime(a)

// TestMigrationThreeNormalizesCoordinationTimestamps
// open a raw *sql.DB, applyMigrations(ctx, db, DialectSQLite, migrations[:2]),
// insert a run row, a run_leases row expires_at "2026-01-01T12:00:01Z",
// a work_queue row available_at/enqueued_at "2026-01-01T12:00:01Z",
// a workers row last_seen_at "2026-01-01T12:00:01.5Z";
// applyMigrations(ctx, db, DialectSQLite, migrations)
want expires_at == "2026-01-01T12:00:01.000000000Z"
want last_seen_at == "2026-01-01T12:00:01.500000000Z"
want SchemaVersion == 3

// TestMigrationThreeRefusesAnUnparseableTimestamp
// as above but expires_at "yesterday"
want error containing "run_leases.expires_at"
want schema version still 2 (the migration rolled back)
```

Add `TestPostgresMigrationThreeNormalizesCoordinationTimestamps` beside the existing PostgreSQL tests in `internal/store/postgres_test.go`, following their `pgtest` pattern, with the same assertions.

- [ ] **Step 4: Run and confirm compile failure**

Run: `go test -count=1 -run 'CoordinationTime|MigrationThree' ./internal/store/`
Expected: FAIL, `undefined: CoordinationTime`.

- [ ] **Step 5: Implement.** Add `timestamp.go`. Replace every timestamp written into the three coordination tables with `CoordinationTime(...)`, including the `stamp` bound into `ClaimRun`'s comparisons and `RenewRunLease`'s `expires_at > ?`. Add the `rewrite` field and migration 3, named `"canonical coordination timestamps"`. It reads every timestamp column of those three tables, parses with `time.RFC3339Nano`, and writes back `CoordinationTime`. On a parse failure it returns an error naming `table.column` and the row key. Set `CurrentSchemaVersion = 3`. Update tests that hard-code the current version as 2 only where they mean "current".

- [ ] **Step 6: Run to verify**

Run: `go test -count=1 ./internal/store/ ./internal/worker/ ./cmd/twinwright/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/store/
git commit -m "Order lease and queue times by time, not by text

RFC3339Nano trims trailing zeros, so 12:00:01Z sorts after 12:00:01.5Z.
SQL compared those strings, letting a renewal revive an expired lease and a
claim miss one. Coordination columns now use a fixed-width RFC 3339 form,
and migration 3 rewrites existing rows."
```

### Task 2: Delivery bound and an honest takeover flag

**Files:**
- Modify: `internal/store/queue.go` (`QueueEntry.Takeover`, `FailRunAtDeliveryLimit`)
- Modify: `internal/worker/worker.go` (`ClaimAndRun`)
- Test: `internal/worker/delivery_test.go`, `internal/store/queue_test.go` (fenced variant; create the file if absent)

**Interfaces:**
- Consumes: Task 1's `CoordinationTime`.
- Produces: `QueueEntry.Takeover bool` (`json:"takeover"`), true only when `ClaimRun` displaced an expired lease (the claimed row was in state `leased`). `func (s *Store) FailRunAtDeliveryLimit(ctx context.Context, lease RunLease, now time.Time, deliveries, maxAttempts int) error`: one transaction that first calls `GuardFenceTx`, then sets `runs.status='failed'`, appends ledger event `error` `{"kind":"delivery_limit","deliveries":deliveries,"max_attempts":maxAttempts}`, sets the queue entry `failed` with `last_error` `delivery limit: <N> deliveries without completion (max attempts <M>)`, releases the lease (`owner=''`), and appends an `OwnershipFailed` log row. `Outcome.Takeover` is copied from `entry.Takeover`.

- [ ] **Step 1: Write the failing tests** in `delivery_test.go`, using `worker_test.go` helpers (`newClock`, `openStore`, `billingManifest`, `seedRun`, `realExecutor`, `quietLogger`):

```go
// TestRequeueAfterAnErrorIsNotReportedAsATakeover
// executor fails on its first call only; Config{MaxAttempts: 3, Backoff: time.Second}
first, _ := w.ClaimAndRun(ctx)  // want Disposition Requeued
clock.Advance(2 * time.Second)
second, _ := w.ClaimAndRun(ctx) // want Attempt 2, Takeover false

// TestExpiredLeaseClaimIsReportedAsATakeover
s.ClaimRun(ctx, "worker-a", time.Minute, clock.Now()); clock.Advance(2 * time.Minute)
outcome, _ := b.ClaimAndRun(ctx) // want Takeover true

// TestRunThatKillsEveryDeliveryIsFailedAtTheDeliveryLimit
// MaxAttempts 3; three times: s.ClaimRun(ctx, fmt.Sprintf("doomed-%d", i), time.Minute, clock.Now()); clock.Advance(2*time.Minute)
// worker-b's executor calls t.Fatal if invoked
outcome, err := b.ClaimAndRun(ctx)
want err == nil, outcome.Disposition == Failed, outcome.Attempt == 4
want run.Status == "failed"
want last event type "error", payload {"kind":"delivery_limit","deliveries":4,"max_attempts":3}
want payload holds no key other than kind, deliveries, max_attempts
want QueueEntryFor(run.ID).State == "failed"
want a following s.ClaimRun returns ErrNoWork

// TestCompletedRunPastTheDeliveryLimitIsFinishedNotFailed
// three doomed claims as above; the third delivery runs agent.Runner to completion
// under its own fence and never calls FinishRun (death at after-completion)
outcome := b.ClaimAndRun(ctx)
want Disposition Finished, run.Status "completed", no delivery_limit event
```

In `internal/store/queue_test.go`:

```go
// TestDeliveryLimitWriteIsFenced
// claim by worker-a, takeover by worker-b after expiry
err := s.FailRunAtDeliveryLimit(ctx, staleLease, now, 2, 1)
want errors.Is(err, ErrFenced); run status unchanged; no new events
```

- [ ] **Step 2: Run and confirm failure**

Run: `go test -count=1 -run 'Takeover|DeliveryLimit' ./internal/worker/ ./internal/store/`
Expected: FAIL. `Takeover` is true after a requeue, the crash-loop run executes, and `FailRunAtDeliveryLimit` is undefined.

- [ ] **Step 3: Implement.** `ClaimRun` sets `entry.Takeover` from the pre-claim state. In `ClaimAndRun`, right after the claim and before starting renewal or `Execute`, when `entry.Attempts > w.Config.MaxAttempts`: read the run. If its status is `completed`, call `FinishRun` and report `Finished`. Otherwise call `FailRunAtDeliveryLimit(ctx, lease, now, entry.Attempts, w.Config.MaxAttempts)` and report `Failed`, with `Err` set to the same reason text. Then observe and return.

- [ ] **Step 4: Run to verify**

Run: `go test -count=1 ./internal/store/ ./internal/worker/ ./internal/bench/ ./cmd/twinwright/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/ internal/worker/
git commit -m "Bound redelivery of a run that kills its worker

A crash never reported an error, so MaxAttempts never applied and a poison
run was taken over forever. Deliveries now count toward the limit, the run
fails with a ledger record of how execution ended, and a takeover means a
displaced lease rather than any second attempt."
```

### Task 3: Crash points in the runner and dispatcher

**Files:**
- Create: `internal/crashpoint/crashpoint.go`, `internal/crashpoint/crashpoint_test.go`
- Modify: `internal/agent/runner.go` (`Runner.Crash`), `internal/dispatch/dispatch.go` (`Dispatcher.Crash`)
- Test: `internal/agent/crashpoint_test.go`, `internal/dispatch/crashpoint_test.go`

**Interfaces:**
- Produces:

```go
package crashpoint
type Point string
const (
    BeforeModelRequest Point = "before-model-request"
    AfterModelRequest  Point = "after-model-request"
    AfterModelResponse Point = "after-model-response"
    BeforeToolCommit   Point = "before-tool-commit"
    AfterToolCommit    Point = "after-tool-commit"
    BeforeCompletion   Point = "before-completion"
    AfterCompletion    Point = "after-completion" // never fired by code; simulated by a harness that skips FinishRun
)
func Points() []Point                    // all seven, in the order above
func ParsePoint(s string) (Point, error) // unknown name -> error listing the valid names
type Site struct { Point Point; RunID string; Step int; CallID, OperationID string }
type Hook func(ctx context.Context, site Site) error
var ErrCrashed = errors.New("simulated process death")
func Fire(ctx context.Context, hook Hook, site Site) error // nil hook -> nil; hook error e -> fmt.Errorf("%w at %s: %v", ErrCrashed, site.Point, e)
```

- `agent.Runner.Crash crashpoint.Hook` fires `BeforeModelRequest` before `startModelCall`, `AfterModelRequest` after it, `AfterModelResponse` after `SaveTurnGuarded`, and `BeforeCompletion` before the `completed` status write. `Site.Step` is `run.Step`.
- `dispatch.Dispatcher.Crash crashpoint.Hook` fires `BeforeToolCommit` immediately before `tx.Commit()` (an error returns and the deferred rollback discards everything), and `AfterToolCommit` immediately after a successful commit. Both carry `CallID` and `OperationID`. On an idempotency-cache hit neither fires.
- Contract: when `Fire` returns an error, the caller returns it at once and writes nothing more. In `Runner.Execute`, the `Invoke` error path checks `errors.Is(err, crashpoint.ErrCrashed)` before recording a dispatch error.

- [ ] **Step 1: Write the failing tests.** `crashpoint_test.go`: `TestFireWithNilHookIsANoOp`, `TestFireWrapsHookErrorsAsCrashes` (want `errors.Is(err, ErrCrashed)` and the point name in the text), `TestParsePointRoundTripsEveryPoint` (every element of `Points()` parses to itself; `"mid-flight"` errors).

`internal/agent/crashpoint_test.go`: `TestCrashPointsLeaveTheDocumentedDurableState`. It runs the billing `ScriptedProvider` duplicate-charge run on SQLite (same setup as `runner_test.go`), crashing at the first occurrence of each point. For tool points it crashes at the first site whose `OperationID == "createRefund"`. For every case: `errors.Is(err, crashpoint.ErrCrashed)`, no `error` event recorded, and run status not `failed`. Then per point:

| Point | Assertions |
|---|---|
| before-model-request | count(model.request) == count(model.response); last event is not model.request |
| after-model-request | last event is model.request; requests == responses + 1 |
| after-model-response | last event is model.response; last transcript message is assistant with at least one tool call and no tool_results row for it |
| before-tool-commit | refunds == 0; no tool.request event with call_id "fixture-refund"; no tool_results row for it |
| after-tool-commit | refunds == 1; tool_results row for "fixture-refund"; last transcript message is role tool, call_id "fixture-refund" |
| before-completion | last transcript message is assistant without tool calls; status != completed; no execution.completed event |

`internal/dispatch/crashpoint_test.go`:
- `TestCrashBeforeToolCommitLeavesNothing`: `Invoke` returns `ErrCrashed`; zero new events, zero tool_results, zero refunds.
- `TestCrashAfterToolCommitIsAnsweredFromTheCacheOnRetry`: `Invoke` returns `ErrCrashed` with refund 1 and a saved result. Re-`Invoke` of the same call ID with a nil hook returns the saved result, refunds stay at 1, and no new events are added.

- [ ] **Step 2: Run and confirm failure**

Run: `go test -count=1 ./internal/crashpoint/ ./internal/agent/ ./internal/dispatch/`
Expected: FAIL, package `crashpoint` not found.

- [ ] **Step 3: Implement** the package and the hook calls described in Interfaces.

- [ ] **Step 4: Run to verify**

Run: `go test -count=1 ./internal/crashpoint/ ./internal/agent/ ./internal/dispatch/ ./internal/worker/`
Expected: PASS.

- [ ] **Step 5: Commit** `internal/crashpoint/`, `internal/agent/`, `internal/dispatch/` with a message saying the crash tests could only cut inside a provider call, and this names every durable boundary so recovery can be tested at each one.

### Task 4: Crash matrix harness and exhaustive matrix

**Files:**
- Create: `internal/crashmatrix/scenario.go`, `matrix.go`, `verify.go`, `matrix_test.go`, `postgres_test.go`

**Interfaces:**
- Consumes: `crashpoint` (Task 3), `QueueEntry.Takeover` and the delivery bound (Task 2), `worker.New`, `replay.Verify`, `checkpoint.List`, `checkpoint.Reconstruct`.
- Produces:

```go
type Target struct {
    Manifest          compiler.Manifest
    Provider          agent.Provider
    Steps             int
    MutatingOperation string         // "createRefund" (billing) or "billing.createRefund" (company)
    ZombieArguments   map[string]any // {"charge_id":"CH-1002","amount_cents":500,"reason":"stale worker retry"}
}
type Scenario struct {
    Name   string // "duplicate-charge", "company-incident", "ambiguous-commit"
    Target Target
    Seed   func(ctx context.Context, s *store.Store) (store.Run, error)
    Judge  func(ctx context.Context, s *store.Store, run store.Run) (bool, []string, error)
}
func Scenarios(examplesRoot string) ([]Scenario, error)
type Case struct {
    Point      crashpoint.Point `json:"point"`
    Occurrence int              `json:"occurrence"`
    Operation  string           `json:"operation,omitempty"`
}
type Reference struct {
    Counts    map[crashpoint.Point]int
    Mutations int
    TableRows map[string]int
}
func Enumerate(ctx context.Context, s *store.Store, sc Scenario) (Reference, []Case, error)
type Clock struct{ /* mutex-guarded time */ }
func NewClock(start time.Time) *Clock
func (c *Clock) Now() time.Time
func (c *Clock) Advance(d time.Duration)
func (c *Clock) Store() store.Clock
func CrashHook(c Case, onCrash func()) crashpoint.Hook
type CrashOutcome struct { Doomed store.RunLease; CrashSeq int; Crashed bool }
func Crash(ctx context.Context, s *store.Store, t Target, runID string, c Case, clock *Clock, ttl time.Duration) (CrashOutcome, error)
func Recover(ctx context.Context, s *store.Store, t Target, clock *Clock, ttl time.Duration) (worker.Outcome, error)
type Result struct {
    Case                      Case     `json:"case"`
    RunID                     string   `json:"run_id"`
    Crashed                   bool     `json:"crashed"`
    CrashedFence              int64    `json:"crashed_fence"`
    RecoveryFence             int64    `json:"recovery_fence"`
    Takeover                  bool     `json:"takeover"`
    Deliveries                int      `json:"deliveries"`
    Mutations                 int      `json:"mutations"`
    DuplicateCallIDs          []string `json:"duplicate_call_ids,omitempty"`
    ReplayVerified            bool     `json:"replay_verified"`
    CheckpointsReconstructed  int      `json:"checkpoints_reconstructed"`
    StaleNewCallRejected      bool     `json:"stale_new_call_rejected"`
    StaleExistingCallRejected bool     `json:"stale_existing_call_rejected"`
    Violations                []string `json:"violations,omitempty"`
}
func (r Result) Passed() bool
func Verify(ctx context.Context, s *store.Store, t Target, ref *Reference, runID string, crash CrashOutcome, outcome worker.Outcome) Result
func RunCase(ctx context.Context, s *store.Store, sc Scenario, ref Reference, c Case) (Result, error)
```

Behaviour:
- `Scenarios`: `duplicate-charge` uses the billing manifest and `agent.ScriptedProvider{}`. `company-incident` uses the company world (`examples/company/world.yaml`) and `agent.CompanyScriptedProvider{Scenario: "company-incident"}`. `ambiguous-commit` uses billing with chaos policy `examples/chaos/ambiguous-commit.yaml` and `agent.AmbiguousScriptedProvider{Unsafe: false}`. Compile manifests and seed runs the way `internal/bench/run.go` does (`SeedScenario`, `CreateRunConfigured`). Each `Judge` is `eval.Evaluate` for its scenario.
- `Enumerate`: runs the scenario once with a counting hook that never crashes. It errors if the reference judge fails. It returns one `Case` per (point, occurrence) for the six fired points, plus `{AfterCompletion, 1}`.
- `CrashHook`: counts sites matching `c.Point`, and `c.Operation` when it is set. At the `c.Occurrence`-th match it calls `onCrash` and returns an error.
- `Crash`: `ClaimRun(ctx, "crashmatrix-doomed", ttl, clock.Now())`, then a fenced `agent.Runner` whose hook (runner and dispatcher) cancels the run context and returns an error. For `AfterCompletion` it runs to completion with no hook and skips `FinishRun`. `CrashSeq` is the highest event seq after the crash.
- `Recover`: advances nothing. It runs one `worker.New(...).ClaimAndRun` as `crashmatrix-recovery` with a fenced runner using `t.Provider`.
- `Verify` appends one human-readable violation per failed check:
  1. crashed (except AfterCompletion);
  2. `Disposition == Finished`, `outcome.Fence > Doomed.Token`, `Takeover`;
  3. run `completed`;
  4. agent mutations: walk events in seq order, take the call from the latest `tool.request`, and attribute each `state.mutation` to it; `chaos.actor_mutation` is not counted. Any call ID with two mutations is a violation; with `ref`, the total must equal `ref.Mutations`;
  5. with `ref`, world-table row counts for this world equal `ref.TableRows`;
  6. `replay.Verify` verified;
  7. `checkpoint.List` non-empty, and `Reconstruct` succeeds for the latest checkpoint with `EventSeq <= CrashSeq` (when one exists) and for the last checkpoint;
  8. model.request count == model.response count;
  9. `Dispatcher{Fence: &doomedFence}.Invoke` returns `ErrFenced`, both for call ID `crashmatrix-zombie` with `MutatingOperation` and for the run's first committed call ID with its recorded operation and arguments; the mutation count is unchanged afterwards.
- `RunCase`: `Seed`, `Enqueue` (steps `t.Steps`), `Crash` with TTL 1 minute on `NewClock(2026-01-01T12:00:00Z)`, `Advance(2*ttl)`, `Recover`, `Verify`; scenario judge failure is a violation.

- [ ] **Step 1: Write the failing tests** in `matrix_test.go`:
- `TestEnumerateCountsEveryBoundary`: for duplicate-charge, every fired point count > 0, `Counts[AfterCompletion] == 1`, and `len(cases) == sum(Counts)`.
- `TestCrashMatrixSQLite`: for each scenario, with `t.Parallel()` per scenario, run every case in a fresh SQLite file under `t.TempDir()` as the subtest `<scenario>/<point>#<occurrence>`. Fail with `strings.Join(result.Violations, "; ")`. Under `testing.Short()`, run only the first and last occurrence of each point.
- `TestVerifyReportsAnExtraMutation` (adversarial): after a clean `RunCase` setup, invoke one extra `createRefund` with a new call ID under the recovery worker's live fence before `Verify`. Want a violation mentioning the mutation count.
- `TestVerifyReportsReplayDivergence` (adversarial): after recovery, `UPDATE tool_results SET body='{}'` for one call. Want a violation mentioning replay.

`postgres_test.go`: `TestCrashMatrixPostgres`, skipped unless `pgtest.Available()`; first and last occurrence per point for all three scenarios, each in `pgtest.SchemaDSN(t)`.

- [ ] **Step 2: Run and confirm failure**

Run: `go test -count=1 ./internal/crashmatrix/`
Expected: FAIL, undefined symbols.

- [ ] **Step 3: Implement** the package to the Interfaces and Behaviour above.

- [ ] **Step 4: Run to verify, full and short**

Run: `go test -count=1 ./internal/crashmatrix/ && go test -count=1 -short ./internal/crashmatrix/`
Expected: PASS, with zero violations across all cases. Record the case count and duration per scenario in the report.

- [ ] **Step 5: Commit** `internal/crashmatrix/` (plus any production fix the matrix forced, each with its regression test), with a message saying the matrix exists because the previous refund-once check passed trivially.

### Task 5: Real process death

**Files:**
- Create: `internal/crashmatrix/process_unix_test.go` (`//go:build unix`)

**Interfaces:**
- Consumes: `Scenarios`, `CrashHook`, `Crash`, `Recover`, `Verify`, `NewClock` (Task 4).

Behaviour:
- `TestMain` in this file. When `TWINWRIGHT_CRASHMATRIX_HELPER=1`, it runs the helper and does not run tests. The helper reads `TW_CM_DSN`, `TW_CM_SCENARIO`, `TW_CM_RUN`, `TW_CM_POINT`, `TW_CM_OCCURRENCE` and `TW_CM_OPERATION`, opens the store, and calls `Crash` with TTL 1 minute and `NewClock(time.Now())`. Its `onCrash` calls `syscall.Kill(os.Getpid(), syscall.SIGKILL)` and then blocks forever. If `Crash` returns without the kill, the helper exits with code 3.
- The parent seeds and enqueues, closes its store, runs `exec.Command(os.Args[0])` with that environment, and requires the `*exec.ExitError` to report `syscall.SIGKILL`.
- It then reopens the store, reads `s.RunLease(ctx, runID)` as `Doomed`, and takes the max event seq as `CrashSeq`. Before recovery it asserts the durable state:
  - `before-tool-commit`: refunds == 0, because the uncommitted transaction died with the process;
  - `after-tool-commit`: refunds == 1.
- It runs `Recover` with `NewClock(time.Now().Add(2 * time.Minute))`, then `Verify` with `ref` from `Enumerate` on a separate store, and requires zero violations.

- [ ] **Step 1: Write `TestRealProcessDeathIsRecovered`**, table-driven over duplicate-charge on a SQLite file under `t.TempDir()`: `{after-model-request, 2}`, `{after-model-response, 3}`, `{before-tool-commit, 1, createRefund}`, `{after-tool-commit, 1, createRefund}`. Add `TestRealProcessDeathIsRecoveredPostgres` (skipped unless `pgtest.Available()`) with the two tool-commit cases.

- [ ] **Step 2: Run and confirm it fails or passes for the right reason**

Run: `go test -count=1 -run RealProcessDeath -v ./internal/crashmatrix/`
Expected: PASS, with the log showing `signal: killed` for each child. The test is new evidence over existing code, so it passing at once is acceptable. Make it fail once by temporarily changing the helper to exit 0 instead of the kill, confirm the SIGKILL assertion catches it, and revert.

- [ ] **Step 3: Commit** with a message saying the in-process matrix cannot show what survives a dead process holding an open transaction, and this does.

### Task 6: Refuse call-ID reuse from a provider

**Files:**
- Modify: `internal/agent/runner.go` (turn validation in `Execute`)
- Test: `internal/agent/callid_test.go`

**Interfaces:** none new. The behaviour change is in turn validation, which already records failures through `FailModelTurnGuarded`.

- [ ] **Step 1: Write the failing tests**, using a stub provider that returns a fixed list of turns:

```go
// TestProviderReusingACallIDAcrossTurnsFailsTheTurn
// turn 1: call {ID:"dup", OperationID:"getCustomer", Arguments:{"id":"C-104"}}
// turn 2: call {ID:"dup", OperationID:"listInvoices", Arguments:{"id":"C-104"}}
_, err := runner.Execute(ctx, run.ID, 10)
want err text contains `reused call ID "dup"`
want run.Status == "failed"; exactly 1 tool_results row; last event "error" whose message contains "dup"

// TestProviderRepeatingACallIDWithinOneTurnFailsTheTurn
// one turn with two calls both ID "twin"
want err text contains `call ID "twin" appears twice in one turn`; zero tool_results rows
```

- [ ] **Step 2: Run and confirm failure**

Run: `go test -count=1 -run CallID ./internal/agent/`
Expected: FAIL. Today the second call is skipped silently and the run does not fail on it.

- [ ] **Step 3: Implement.** Collect the call IDs of every assistant message already in `history`, and check each new call against that set and against the IDs earlier in the same turn. Use exactly the two messages above, through the existing validation-error path.

- [ ] **Step 4: Run to verify**

Run: `go test -count=1 ./...`
Expected: PASS. No existing fixture reuses an ID; if one does, report it rather than editing the fixture.

- [ ] **Step 5: Commit** with a message saying a reused ID was silently dropped, so the model never received a result for that call.

### Task 7: Fenced, renewed, released local execution for `run` and `resume`

**Files:**
- Create: `internal/worker/local.go`, `internal/worker/local_test.go`
- Modify: `cmd/twinwright/main.go` (the local path of `run` without `--enqueue`, and `resume`)
- Test: `cmd/twinwright/main_test.go` (one new test)

**Interfaces:**
- Produces:

```go
const DefaultLocalLeaseTTL = 15 * time.Second
func LocalOwner() string // "local-<hostname>-<pid>"
// ExecuteLocal acquires a lease on runID (owner cfg.ID, default LocalOwner(); TTL cfg.LeaseTTL, default DefaultLocalLeaseTTL),
// renews it every cfg.RenewEvery while fn runs (reusing the worker's renewal loop; a lost renewal cancels fn's context),
// and releases it when fn returns, using context.WithoutCancel so cancellation still releases.
// A live lease held by another owner returns an error wrapping store.ErrLeaseHeld that names the owner and expiry; fn is not called.
// A lost lease returns an error wrapping store.ErrFenced.
func ExecuteLocal(ctx context.Context, s *store.Store, runID string, cfg Config, fn func(ctx context.Context, fence store.Fence) (store.Run, error)) (store.Run, error)
```

- CLI: `run` (local path) and `resume` wrap their context in `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` and call `ExecuteLocal`. Inside, the `agent.Runner` and `dispatch.Dispatcher` get `Fence: &fence`. JSON output and exit codes are unchanged on success.

- [ ] **Step 1: Write the failing tests** in `local_test.go`:
- `TestExecuteLocalFencesItsWrites`: inside `fn`, a dispatcher with the fence commits `getCustomer`. Afterwards `s.RunLease(ctx, id).Owner == ""` and the token is 1.
- `TestExecuteLocalRefusesARunLeasedByAFleetWorker`: after `Enqueue` and `ClaimRun("worker-a", time.Minute)`, want `errors.Is(err, store.ErrLeaseHeld)`, error text containing `worker-a`, and `fn` never called.
- `TestFleetCannotClaimARunExecutingLocally`: with the run enqueued, inside `fn` `s.ClaimRun(ctx, "worker-b", time.Minute, time.Now())` returns `store.ErrNoWork`.
- `TestExecuteLocalRenewsItsLease`: real clock, `LeaseTTL: 2*time.Second`, `RenewEvery: time.Second`. `fn` sleeps 3s, then `s.GuardFence(ctx, fence, time.Now())` returns nil.
- `TestExecuteLocalReleasesTheLeaseWhenCancelled`: `fn` cancels the parent context and returns `ctx.Err()`. Want `errors.Is(err, context.Canceled)` and the lease owner `""` afterwards.

In `main_test.go`, `TestResumeRefusesARunLeasedByAWorker`: create a run with `--enqueue` through the CLI entry point the existing tests use, claim it in the store as `worker-a`, then `resume` must error with text containing `leased by`.

- [ ] **Step 2: Run and confirm failure**

Run: `go test -count=1 -run 'ExecuteLocal|ExecutingLocally|ResumeRefuses' ./internal/worker/ ./cmd/twinwright/`
Expected: FAIL, `undefined: ExecuteLocal`.

- [ ] **Step 3: Implement** as specified in Interfaces.

- [ ] **Step 4: Run to verify**

Run: `go test -count=1 ./internal/worker/ ./cmd/twinwright/ && ./scripts/e2e.sh`
Expected: PASS, and `e2e: every step passed`.

- [ ] **Step 5: Commit** with a message saying local execution was unfenced and could run alongside a fleet worker on the same run.

### Task 8: Bench distributed mode on the crash matrix

**Files:**
- Modify: `internal/bench/suite.go` (`Case.CrashPoint string yaml:"crash_point"`, `Case.CrashOperation string yaml:"crash_operation"`; both are refused outside `mode: distributed`; `crash_point` must pass `crashpoint.ParsePoint`; `crash_at` stays the occurrence and must be at least 1)
- Modify: `internal/bench/modes.go` (`runDistributed` delegates to `crashmatrix.Crash`, `Recover` and `Verify` with `ref` nil; delete the local `crashAtProvider` and `benchClock` that the harness replaces)
- Modify: `internal/bench/report.go` (`DistributedResult` gains `CrashPoint string json:"crash_point"`, `CrashOperation string json:"crash_operation,omitempty"`, `Occurrence int json:"occurrence"`, `Violations []string json:"violations,omitempty"`; a case with violations is an error case)
- Modify: `examples/bench/standard.yaml`. Fix the comment that says turn 4 dies with the refund already committed: in the billing fixture the refund commits only before model turn 5, so no mutation has committed at turn 4 (verify by counting `state.mutation` events at that point). Add a case `distributed-crash-after-refund-commit` with `world: billing`, `scenario: duplicate-charge`, `mode: distributed`, `crash_point: after-tool-commit`, `crash_operation: createRefund`, `crash_at: 1`, `expect: pass`, category `distributed`.
- Test: `internal/bench/run_test.go`, `internal/bench/suite_test.go`

- [ ] **Step 1: Write the failing tests:**
- `TestIntegerCrashAtKeepsItsMeaning`: a distributed case with only `crash_at: 3` parses to `CrashPoint == ""`, which means `after-model-request` occurrence 3. Its result reports `crash_point` `after-model-request` and `occurrence` 3, with the same takeover, fences and replay verdict as before.
- `TestCrashPointOutsideDistributedModeIsRefused`: `crash_point` on a local case gives a parse error naming the field.
- `TestUnknownCrashPointIsRefused`: `crash_point: mid-flight` gives a parse error.
- In `TestRunStandardSuiteScripted`, update the case count from 20 to 21. For `distributed-crash-after-refund-commit`, want `Distributed.Takeover`, `Distributed.ReplayVerified`, `AgentRefunds == 1`, no violations and status passed.
- `TestFlagshipSuiteRecoversEveryCrashCase`: runs `examples/bench/flagship.yaml` with the scripted agent; every case is `as_expected`, and every distributed case reports `replay_verified` with no violations.

- [ ] **Step 2: Run and confirm failure**

Run: `go test -count=1 ./internal/bench/`
Expected: FAIL. The fields are unknown and the new case is missing.

- [ ] **Step 3: Implement** as listed under Files.

- [ ] **Step 4: Run to verify**

Run: `go test -count=1 ./internal/bench/ ./cmd/twinwright/ && go run ./cmd/twinwright demo --dir "$(mktemp -d)/demo"`
Expected: PASS, and the demo ends with `Every step verified its own claim.`

- [ ] **Step 5: Commit** with a message saying the bench's only post-mutation crash ran outside `go test`, and the suite's comment claimed a committed refund that was not there.

### Task 9: Publish the contract

**Files:**
- Create: `docs/adr/0029-crash-boundaries-and-delivery-bounds.md`, following the section layout of `docs/adr/0027-provider-deadlines-and-truncation.md`
- Modify: `docs/distributed.md`. Add a "Crash boundaries" section with a table: point | durable state at death | proven by. Name `TestCrashMatrixSQLite`, `TestCrashMatrixPostgres` and `TestRealProcessDeathIsRecovered` (with which points the real-process test covers), and keep the diagrams test in `internal/store/diagrams_test.go` passing.
- Modify: `docs/adr/0020-multi-worker-runtime.md`. Add one line under the title: `Amended by ADR 0029: crash coverage, delivery bound, idempotency scope.` Do not rewrite its body.
- Modify: `README.md` (Distributed runtime > Semantics and Current limitations), `CHANGELOG.md` (an `Unreleased` section), and `docs/security-review.md` only if a cited test was renamed.

Content the ADR must state:
- the seven boundaries and the durable state at each;
- the timestamp layout and migration 3;
- the delivery bound and its ledger event;
- local execution under a lease, and refused call-ID reuse;
- the idempotency statement from the spec, verbatim in meaning: a world mutation commits at most once per (run, call ID); model calls are at-least-once and a crash before a recorded response repeats the provider call and its cost; a retry with a new call ID is a new action; provider spend, logs and metrics are not fenced;
- the corrections to ADR 0020, which overstated three things: "every model turn" crash coverage, the call-ID cache being load-bearing during recovery, and the lease unit tests running against PostgreSQL;
- remaining limits: `fork`, `counterfactual` and `bench` execute their own new runs unfenced; the real-process test kills processes one at a time and does not show concurrent multi-process SQLite contention; the matrix covers three scenarios.

- [ ] **Step 1: Write the documents.**
- [ ] **Step 2: Verify**

Run: `make check && go test -count=1 ./... && go test -race -count=1 ./... && ./scripts/e2e.sh && make bench-smoke && go mod tidy && git diff --exit-code go.mod go.sum && ! grep -n "—" docs/adr/0029-*.md docs/distributed.md CHANGELOG.md README.md`
Expected: every command exits 0.

- [ ] **Step 3: Commit** the docs, with a message saying the published guarantees now match what the tests prove, including where ADR 0020 overstated them.
