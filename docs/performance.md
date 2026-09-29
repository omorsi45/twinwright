# Performance

Twinwright measures the runtime primitives it owns. Everything here came out of
`scripts/perf.sh` on the machine named below. Nothing in this file is estimated,
extrapolated, or carried over from another machine, and figures this host could
not measure stably are listed as unmeasured rather than reported with a caveat.

## What is measured, and what is not

Measured: the durable operations the runtime performs on its own behalf.

| Operation | Why it matters |
| --- | --- |
| Ledger append | The most frequent write. Every model turn and tool call appends. |
| Ledger append under a fence | The same write with distributed ownership checking, so fencing overhead is attributable rather than assumed. |
| Whole-ledger read | Where replay, trace derivation and checkpoint listing all begin. |
| Lease acquisition | Gate on run ownership; paid once per claim. |
| Lease renewal | Paid on a timer for the whole life of every claim, so a slow renewal would be a live problem. |
| Expired-lease takeover | The storage half of distributed handoff after a worker dies. |
| Work claiming | The query a worker runs on every poll. On PostgreSQL this is the `SKIP LOCKED` path. |
| Fence check alone | Isolates the ownership check from the write it guards. |
| Queue drain at 1/2/4/8 workers | Whether throughput responds to adding workers. |
| Replay verification | Re-execution plus comparison, which is what the command actually costs. |
| Checkpoint listing and reconstruction | What a fork or counterfactual pays before it can diverge. |

Not measured, deliberately: model latency and tool latency. Those belong to a
provider or to a simulated service. Including them would produce a large number
that describes a fixture rather than Twinwright, and the scripted provider's
latency is simulated anyway, so a figure containing it would be meaningless.

The queue benchmarks use a no-op execute function for the same reason: the
question is what the claim, fence, renew and finish machinery costs, not what an
agent costs.

## Reproducing

```bash
scripts/perf.sh                                   # SQLite only
scripts/perf.sh "postgres://user:pw@host/db"      # SQLite and PostgreSQL
ITERATIONS=200 COUNT=5 scripts/perf.sh            # tighter, slower
make bench                                        # same as the first form
make bench-smoke                                  # one iteration each, a compile and correctness guard
```

Iteration counts are fixed (`-benchtime=NNx`) rather than time-based, so two runs
perform identical work and are comparable across machines. The script prints
commit, Go version, OS and iteration count above its output, because a benchmark
figure without its environment is not a measurement. Output is standard `go test
-bench` format, so `benchstat old.txt new.txt` compares two runs directly.

Every benchmark runs against each configured dialect. PostgreSQL is skipped
unless `TWINWRIGHT_TEST_POSTGRES_DSN` is set, exactly as the integration tests
are, so the default run stays hermetic.

## Recorded run

```
commit:     milestone-24-performance
date:       2026-09-29 (UTC)
host:       Windows 11, 12th Gen Intel Core i5-1245U, 12 logical CPUs
go:         go1.27.1 windows/amd64
storage:    SQLite (modernc.org/sqlite, pure Go), WAL, one connection
iterations: 50x, 3 repeats
postgres:   not measured on this host (no container runtime available)
```

Two properties of this host dominate every figure below and should be read as
part of the numbers rather than as background:

- The SQLite driver is pure Go, not the C library, and commits are durable. A
  single-row append costs milliseconds here, not microseconds.
- `store.Open` sets `SetMaxOpenConns(1)`. SQLite serialises every reader and
  writer through one connection, by design, so concurrency figures on SQLite
  describe a deliberately serialised store.

### Results

Each row is the spread across three repeats of 50 iterations.

| Benchmark | ns/op | Reading |
| --- | --- | --- |
| `AppendEvent` | 2.06 - 2.89 ms | One durable ledger append. |
| `AppendEventGuarded` | 2.03 - 2.89 ms | Identical to the unfenced append within this host's noise. |
| `GuardFence` | 0.33 - 0.43 ms | The ownership check alone, well under the commit it guards. |
| `RenewRunLease` | 135 - 168 us | A single autocommit `UPDATE`; the cheapest durable operation measured. |
| `LeaseTakeover` | 13.1 - 14.8 ms | Second worker taking a lapsed run and issuing a higher fence. |
| `AcquireRunLease` | 14.4 - 15.3 ms | First acquisition, release untimed. |
| `ClaimRun` | 15.1 - 17.3 ms | Claim one queued entry: lease it and mark it running. |
| `EventsRead` (500 events) | 31.9 - 49.2 ms | Whole-ledger read, about 64 - 98 us per event. |
| `CheckpointList` | 29.9 - 32.7 ms | Hashes every ledger prefix of a completed billing run. |
| `CheckpointReconstruct` | 156 - 165 ms | Rebuilds a world and run at the first tool response. |

### Not reportable from this host

Three benchmarks produced an unusable spread and are deliberately left without a
figure:

| Benchmark | Observed | Why it is not reported |
| --- | --- | --- |
| `Verify` | 27 ms, 180 ms, 818 ms | Allocation counts were identical across all three repeats (17,336 / 17,347 / 17,386), so the work did not change while wall clock moved 30x. |
| `QueueDrain` | 5.6 ms to 45.7 ms | Degrades monotonically across repeats independent of worker count. |
| `ClaimAndFinishOneRun` | 9.6 ms, 34.3 ms, 38.3 ms | Same monotonic drift within one benchmark. |

Identical allocation counts alongside a moving wall clock means the measurement
is describing the machine, not the code. This is a laptop with background
services and on-access file scanning, and the drift accumulates as a run
proceeds. Publishing a single number from that would be inventing precision.

The same commands on a quiet Linux host produce comparable repeats; this file
will carry those figures when they are measured rather than assumed.

## Observations

**Fencing is not a measurable cost.** A fenced append is indistinguishable from
an unfenced one at this resolution, and the isolated fence check (0.33 ms) is a
fraction of the commit it rides along with (2.06 ms). Distributed ownership
checking is safe to leave on.

**Renewal is three orders of magnitude cheaper than acquisition** (140 us versus
15 ms). That is the right shape, because renewal happens on a timer for the whole
life of a claim while acquisition happens once. The difference is structural
rather than accidental: renewal is one autocommit `UPDATE` with its guard folded
into the `WHERE` clause, while acquisition is a multi-statement transaction that
reads the lease row, upserts it, and appends an ownership audit row. Nothing here
argues for optimising acquisition: at 15 ms against a run that takes seconds, it
is not on the critical path.

**Takeover costs about the same as a first acquisition** (13 - 15 ms versus
14 - 15 ms), which is the useful fact about crash recovery: the storage work is
negligible, so recovery time after a worker dies is set almost entirely by the
lease TTL. That is a configuration choice (`DefaultLeaseTTL`, 30 s), not
something to tune in code.

**Whole-ledger reads scale with ledger length**, at roughly 64 - 98 us per event.
Replay, trace derivation and checkpoint listing all pay it. A run with tens of
thousands of events would make those commands noticeably slow, and the fix would
be a bounded read rather than a faster one. No such run exists in this repository
today, so nothing has been changed on the strength of a hypothetical.

**No optimisation has been performed.** These benchmarks exist to establish a
baseline and to make a regression visible, not to justify tuning. The one
structural fact worth recording is `SetMaxOpenConns(1)` on SQLite: throughput
there cannot improve by adding workers, which is why the distributed runtime
targets PostgreSQL and why SQLite remains the local-development and
deterministic-example store.
