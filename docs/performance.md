# Performance

Twinwright measures the runtime primitives it owns. Everything before the
"Platform benchmarks" section came out of `scripts/perf.sh` on the machine named
below; everything from that section on came out of `twinwright perf` and is copied
from the reports committed in `docs/benchmarks/`. Nothing in this file is estimated,
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

## Platform benchmarks

Everything above times single storage operations. `twinwright perf` times the
platform the way a user meets it: a fleet of workers draining a queue, a crashed
worker's run finished by another worker, replay verification, and the whole crash
matrix. It sits beside `scripts/perf.sh` and does not replace it; ADR 0034 records
why both exist.

All four workloads run the scripted `duplicate-charge` scenario, the one the crash
matrix and the benchmark suite run, with the scripted provider. No model, no tool
service and no network call is involved, so a figure describes Twinwright and its
database, not an agent.

| Workload | What one measurement is |
| --- | --- |
| `throughput` | For each fleet size and each trial: a fresh store, `--runs` seeded runs queued, then that many in-process workers (`worker.Worker`, a real clock, a 30 s lease, a 10 ms poll), each running its claims under that claim's fence, which is the wiring `twinwright worker` uses. The result is `--runs` divided by the wall time from just before the first worker starts until the last one returns, which it does when nothing is claimable. Every run must end finished and completed, or the trial is an error. |
| `recovery` | For each lease TTL and each trial: one run is queued and a first worker is crashed in process at `after-tool-commit` on `createRefund`, just after the refund commits. The dead worker's lease is renewed at that instant, so it expires one TTL later. The result is the time from that instant until a second worker, polling every 100 ms, reports the run finished and completed through a takeover. `duplicate_mutations` counts committed refunds beyond the one the run owns, summed over the trials, and must be 0. |
| `replay` | `--replay-runs` runs are executed to completion, then `replay.Verify` is timed on each. The statistics are over those per-run times, not over trials. |
| `crash-matrix` | Every `duplicate-charge` crash case is enumerated and run to recovery. The result is the case count, the violations summed over the cases, and the wall time of the whole pass, enumeration and per-case store setup included. It is one pass, so it has no spread. |

### Running it

```bash
twinwright perf --out perf.json                              # every workload, temporary SQLite file
twinwright perf --db "postgres://user:pw@host/db" --out perf.json
make perf                                                    # the first form, via go run, into the git-ignored perf.json
```

The defaults are fleet sizes 1,2,4,8, 40 runs per trial, 3 trials, lease TTLs
1s,2s,5s, 20 replay runs, seed 42 and all four workloads. `--out` is required. The
command writes the report, prints a short summary, and then exits 1 if a recovery
duplicated a committed mutation or the crash matrix reported a violation, or 2 for
bad input.

The PostgreSQL figures come from the `perf` job of
`.github/workflows/integration.yml`. It runs only on `workflow_dispatch`, never on a
push or a pull request. It starts a `postgres:17` service container on an
`ubuntu-latest` runner, builds the binary, writes the runner's CPU count, CPU model,
memory, Go version and (when `psql` is available) PostgreSQL server version to
`runner.txt`, runs `twinwright perf --db "$DSN" --trials 3 --out perf.json`, and
uploads both files as the artifact `perf-postgres17`.

## Methodology

- **Scripted and seeded.** The runs perf seeds itself use the world seed `--seed`
  (42 by default). The crash matrix keeps its own fixed seed, so the matrix measured
  is the one the test suite runs. The agent is scripted, so every trial does the same
  work.
- **Trials.** Each throughput and recovery measurement repeats `--trials` times (3 in
  both reports below) and is reported as the minimum, median and maximum over the
  trials. With 3 trials the median is the middle one. Values are rounded to three
  decimals. Three trials show a spread; they are not a confidence interval, and no
  statistical test was run.
- **In process, real clock.** Workers are goroutines of the one process that times
  them, on a real clock. Recovery latency is therefore in process, not the latency of
  a killed operating system process.
- **A fresh store where the backend allows one.** On SQLite each throughput trial and
  each crash matrix case gets a new temporary file, while recovery and replay share
  one file for the whole run. On PostgreSQL every trial shares the one database, and
  each starts only when the work queue is idle, so later trials run against the rows
  earlier ones left. The effect of that on the figures was not measured.
- **The environment travels with the figures.** Each report records the build
  version, git revision and whether the tree was modified, the Go version, `goos`,
  `goarch`, the CPU count, the seed and the start time (UTC), plus the backend and the
  measurement settings. The host operating system, CPU model, memory and PostgreSQL
  server version are not in the JSON; they are quoted beside each report below, from
  what was read off the machine or, for the runner, from its `runner.txt`.
- **The database address is never recorded.** The report names the backend (`sqlite`
  or `postgres`), not the DSN, and an error from a failed run has the DSN removed
  from its text.
- **Where published figures come from.** A figure in the Results section is copied
  from a report committed unchanged in `docs/benchmarks/`. The file name states the
  date and the machine. `TestBenchmarkDocsMatchTheirReports` reads those reports and
  the throughput table below on every `go test ./...` and fails if a row differs from
  the report it cites. The other tables were checked against the same reports by a
  script when they were written; no test reads them.

## Results

Each subsection is one report. The tables copy it without rounding. The prose after
them rounds nowhere either, and says only what the reports show.

### SQLite on a Windows PC under WSL2

- Report: `docs/benchmarks/2026-10-28-windows-wsl2-sqlite.json`
- Command: `twinwright perf --trials 3 --out perf.json`, every other setting at its default.
- Backend: SQLite, a temporary file per trial on the WSL2 ext4 virtual disk (`/tmp`).
- Commit: `f718919`, built from a clean tree (`git_modified` is false).
- Go: go1.27.3 linux/amd64. The report says `linux` because the binary ran inside WSL2.
- Machine: Windows 11 Enterprise 10.0.26300 on an AMD Ryzen 9 7900X (12 cores, 24
  threads). The CLI ran in WSL2 Ubuntu 24.04 (kernel 6.18.33.2-microsoft-standard-WSL2)
  with about 15 GiB of memory visible to it. The report records 24 CPUs.
- Load: Docker Desktop and other desktop software were running, so the machine was
  not idle.
- Started: 2026-10-28T18:07:35Z. Trials: 3.

| Workers | Runs per trial | Runs/s median | Runs/s min | Runs/s max |
| --- | --- | --- | --- | --- |
| 1 | 40 | 6.65 | 5.684 | 7.076 |
| 2 | 40 | 5.58 | 5.298 | 5.661 |
| 4 | 40 | 6.673 | 5.856 | 8.027 |
| 8 | 40 | 12.58 | 10.335 | 12.601 |

| Lease TTL (ms) | Recovery ms median | Recovery ms min | Recovery ms max | Duplicate mutations |
| --- | --- | --- | --- | --- |
| 1000 | 1033.215 | 1032.711 | 1034.264 | 0 |
| 2000 | 2044.824 | 2038.42 | 2048.598 | 0 |
| 5000 | 5054.524 | 5052.703 | 5063.707 | 0 |

| Replay runs | Verified | Replay ms median per run | Replay ms min | Replay ms max |
| --- | --- | --- | --- | --- |
| 20 | 20 | 7.761 | 7.297 | 8.695 |

| Crash matrix scenario | Cases | Violations | Wall ms |
| --- | --- | --- | --- |
| duplicate-charge | 25 | 0 | 4598.051 |

### PostgreSQL 17 on a GitHub runner

- Report: `docs/benchmarks/2026-10-28-github-postgres17.json`
- Source: Integration workflow run 40743500240, job "platform benchmarks on
  PostgreSQL 17 (on demand)", artifact `perf-postgres17`.
- Command: `./twinwright perf --db "$DSN" --trials 3 --out perf.json`, every other
  setting at its default. The DSN is not recorded.
- Backend: PostgreSQL 17.11 (Debian 17.11-1.pgdg13+2), a `postgres:17` service
  container on the same runner as the workers.
- Commit: `df3b273`, built from a clean tree (`git_modified` is false).
- Go: go1.27.3 linux/amd64.
- Machine: a GitHub-hosted `ubuntu-latest` runner with 4 vCPUs, an AMD EPYC 7763
  64-Core Processor and about 16 GB of memory (`MemTotal: 16367520 kB`), reported by
  `uname` as `Linux runnervm48cm3 7.0.0-1017-azure #17~24.04.1-Ubuntu SMP PREEMPT Thu Sep 24 18:46:57 UTC 2026 x86_64 x86_64 x86_64 GNU/Linux`.
  It is a virtual machine on shared infrastructure, and what else ran on the host is
  not known.
- Started: 2026-10-28T18:16:42Z. Trials: 3.

| Workers | Runs per trial | Runs/s median | Runs/s min | Runs/s max |
| --- | --- | --- | --- | --- |
| 1 | 40 | 26.75 | 24.311 | 26.757 |
| 2 | 40 | 41.532 | 19.273 | 42.349 |
| 4 | 40 | 60.863 | 56.607 | 62.208 |
| 8 | 40 | 86.481 | 70.864 | 87.719 |

| Lease TTL (ms) | Recovery ms median | Recovery ms min | Recovery ms max | Duplicate mutations |
| --- | --- | --- | --- | --- |
| 1000 | 1023.575 | 1022.916 | 1024.84 | 0 |
| 2000 | 2036.429 | 2035.146 | 2037.639 | 0 |
| 5000 | 5079.41 | 5079.252 | 5081.03 | 0 |

| Replay runs | Verified | Replay ms median per run | Replay ms min | Replay ms max |
| --- | --- | --- | --- | --- |
| 20 | 20 | 22.714 | 21.698 | 29.398 |

| Crash matrix scenario | Cases | Violations | Wall ms |
| --- | --- | --- | --- |
| duplicate-charge | 25 | 0 | 2839.869 |

### What the figures show

**SQLite throughput did not scale with workers on this machine.** The medians were
6.65, 5.58, 6.673 and 12.58 runs/s for 1, 2, 4 and 8 workers: 2 workers were slower
than 1, and 8 were faster than 4. The SQLite store uses a single connection
(`SetMaxOpenConns(1)` in `internal/store/store.go`), which serializes writes. The
cause of the 8-worker figure was not investigated, and one machine with three trials
is not enough to explain it, so no explanation is offered here. The Observations
section above says that throughput on SQLite cannot improve by adding workers. This
run does not support that sentence as written: 8 workers drained at 12.58 runs/s
against 6.65 for one, and the medians do not rise steadily with the worker count
either.

**PostgreSQL throughput rose with workers on the runner.** The medians were 26.75,
41.532, 60.863 and 86.481 runs/s for 1, 2, 4 and 8 workers. The three 2-worker
trials spread from 19.273 to 42.349 runs/s, and the slowest of them was slower than
every 1-worker trial, whose minimum was 24.311. The cause of that spread was not
investigated.

**Recovery tracked the lease TTL on both backends, with no duplicated mutation.**
For TTLs of 1000, 2000 and 5000 ms the medians were 1033.215, 2044.824 and 5054.524
ms on SQLite and 1023.575, 2036.429 and 5079.41 ms on PostgreSQL. In every trial of
both reports the time was above its TTL and below the TTL plus 100 ms.
`duplicate_mutations` is 0 in all six rows.

**Replay and the crash matrix held on both backends.** Replay verified 20 of 20 runs
on each. The crash matrix ran 25 `duplicate-charge` cases with 0 violations on each.

**Read the two reports separately.** They are not a SQLite versus PostgreSQL
comparison. The machines differ (a 24-thread desktop that was not idle, and a shared
4-vCPU virtual machine) and so do the operating system layers (WSL2 on Windows, and a
Linux virtual machine). A figure from one table says nothing about the other.

**What a figure can and cannot claim.**

- GitHub runner figures are from a shared virtual machine and can vary between runs.
  This is one run; how much it varies from one run to the next was not measured.
- Recovery latency is in process with a real clock. The recovery worker polls every
  100 ms, so the figure includes up to one poll interval.
- The real-process crash evidence is milestone 1's `SIGKILL` test,
  `TestRealProcessDeathIsRecovered` (ADR 0029). It shows the run is recovered
  correctly after a killed process. It does not time anything.

## Not measured

- Real-process recovery latency. The recovery workload crashes a worker in process.
- Multi-process or multi-host fleets. Every worker in a trial is a goroutine of one
  process.
- Network latency between the workers and the database. The workers and the database
  were on the same machine in both reports.
- Live model providers. The agents are scripted.
- Memory and CPU usage. Neither is recorded.
- Long-running stability. A trial lasts seconds.
- Anything on macOS.
- PostgreSQL on the local machine, and SQLite on the runner.
- Run-to-run variation. Each report is one run.
- Whether PostgreSQL trials that share one database are affected by the rows earlier
  trials left.
