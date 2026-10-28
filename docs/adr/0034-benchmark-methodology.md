# 0034. Benchmark methodology: what the platform report measures and how its figures are published

Date: 2026-10-28

Status: accepted; extends ADR 0020 and ADR 0029

## Context

`scripts/perf.sh` times single storage operations with `go test -bench`: one ledger
append, one lease renewal, one claim. That finds a regression in one call. It does
not answer what someone who runs the platform asks. How many runs per second does a
fleet of workers drain? After a worker dies, how long until another finishes its
run, and does that follow the lease? What does replay cost? How long does the crash
matrix take? ADR 0020 and ADR 0029 made claims about recovery and delivery, and neither
carried a measured time. `docs/performance.md` had said, correctly, that its own
queue-drain benchmarks were not reportable on the host that ran them.

The second problem is how a number gets into a document. A figure typed in from
memory, with no record of the machine, the build or the backend, cannot be checked by
anyone, including the person who typed it. `docs/performance.md` already treats that
as the main hazard. This ADR applies the same rule to the platform workloads and
makes it something a test can fail on.

## Decision

### Why `twinwright perf` exists beside `scripts/perf.sh`

They measure at different levels and stay separate. `scripts/perf.sh` runs
fixed-iteration benchmarks of single operations and prints output `benchstat` can
compare. `twinwright perf` runs whole workloads and writes one JSON report that names
what produced it. It is a subcommand rather than a benchmark for that reason: its
product is a report that can be committed, read by a test and quoted, and the same
binary can be pointed at a database of the operator's own with `--db`. Before each
throughput trial, recovery trial and crash matrix case it refuses a database whose
work queue is not idle, because the recovery workload and the crash matrix claim the
oldest claimable run and would otherwise take someone else's.

### The four workloads, exactly

All four run the scripted `duplicate-charge` scenario, the one the crash matrix and
the benchmark suite run, so a figure describes code a test already proves correct.
`internal/perf` is the definition; this is what it does.

- **`throughput`.** For each fleet size in `--workers` (default 1,2,4,8) and each of
  `--trials` trials (default 3): take a store, seed and enqueue `--runs` runs (default
  40), each in a world of its own, start that many in-process `worker.Worker`s on a
  real clock with a 30 s lease and a 10 ms poll, and let each claim and run until
  nothing is claimable. Each run executes under the fence of the claim that holds it,
  which is the wiring `twinwright worker` uses. The trial's result is `--runs` divided
  by the wall time from just before the first worker starts to the return of the last,
  and worker registration is inside that time. Every run must end finished and
  completed and the count must equal what was queued, or the trial is an error.
- **`recovery`.** For each lease TTL in `--lease-ttls` (default 1s, 2s, 5s; at least
  one second, which is the shortest lease the store accepts) and each trial: seed and
  enqueue one run, register a second worker that polls every 100 ms, and crash the
  first with `crashmatrix.CrashWith` at `after-tool-commit` on `createRefund`,
  occurrence 1, which is just after the refund commits. The dead worker's lease is then
  renewed at that instant, so it expires exactly one TTL later, whatever time the run
  took to reach its crash point. The result is the time from that instant until the
  second worker's claim ends finished, with the run completed and the claim a takeover
  of the expired lease. The wait gives up after five TTLs plus ten seconds. The refunds in the
  run's world beyond one are counted as `duplicate_mutations` and summed over the
  trials; a world with no refund at all is an error, not a zero.
- **`replay`.** Execute `--replay-runs` runs (default 20) to completion without a
  fence, then time `replay.Verify` on each. The statistics are over those per-run
  times. `verified` counts the runs whose replay reported verified.
- **`crash-matrix`.** Enumerate every `duplicate-charge` crash case, then run each to
  recovery with `crashmatrix.RunCase`. The result is the case count, the violations
  summed over the cases, and the wall time of the whole pass, enumeration and the
  per-case store setup included. It is one pass and has no spread.

The command writes the report, prints a short summary, and then exits 1 if any recovery
duplicated a committed mutation or the matrix reported a violation, and 2 for bad
input. Speed has no pass mark. A mark would have to come from a measurement, and the
two published so far are one run each on two different machines.

`duplicate_mutations` of 0 says the refund was not committed twice in those trials.
Delivery stays at-least-once (ADR 0029); this is not a claim of exactly-once.

### Fixed seed and a scripted scenario

The runs perf seeds itself use `--seed` (default 42) and the scripted provider, so every
trial does the same work and a difference between two figures is a difference in
machine, backend or load, not in what an agent said. The crash matrix keeps its own
fixed seed, so the matrix measured is the one the test suite runs. This makes the work
repeatable. It does not make the timing repeatable, and nothing here claims it does.

### Trials, and what a statistic is

Each throughput and recovery measurement repeats `--trials` times and is reported as the
minimum, median and maximum. The median of an even count is the mean of the two middle
values; of three trials, the middle one. Values are rounded to three decimals, because
finer digits are timer noise. Three trials show a spread. They are not a confidence
interval, and no statistical test is run. A spread that looks wide (it did, once, in the
first PostgreSQL report) is reported as it is, not explained.

### The environment block, and what it leaves out

Each report records the build version and git revision, whether the tree was modified,
the Go version, `goos`, `goarch`, the CPU count, the seed and the start time in UTC,
beside the backend and the settings. Those are what the binary can read about itself.
The host operating system and its version, the CPU model, the memory, the PostgreSQL
server version and how busy the machine was are not in the report. Each published
report is documented with them, read off the machine when it was taken, or, for the
runner, from the `runner.txt` the job writes beside the report. That file is uploaded
with the artifact and is not committed, so those details can be read but no test holds
them.

### The database address is never recorded

`Config.DSN` is excluded from the report's JSON, the report states `backend` as `sqlite`
or `postgres`,
and the copy of the config in the report has the DSN cleared. An error from `perf.Run`
has the DSN removed from its text, with its password and any credential-like query
value, before the command prints it, while still unwrapping to the original cause.
A PostgreSQL DSN that does not parse as a URL, such as one with an unencoded `/`, `#`
or `?` in the password, gets the cause's text replaced entirely by a fixed message
about the `--db` value, because `url.Parse` quotes what it rejected, a piece of the
password among it, and no search for the whole DSN finds that piece.
`TestPerfReportShape` and `TestPerfCLIWritesTheReport` require that the report holds no
`dsn` key and does not mention one, `TestConfigJSONUsesMillisecondsAndOmitsPrivateFields`
holds the config's wire form, and `TestScrubErrorRemovesTheDSN`,
`TestRunNeverPrintsAMalformedDSN` and `TestPerfCLINeverPrintsTheDSN` hold the error
text. The last two use DSNs whose unscrubbed error carries the password, so they fail
when the scrubber is removed; `TestRunNeverPrintsTheDSNOnAConnectionFailure` does not
prove that, because a refused connection never puts the password in the error.

### Where published figures come from, and how they are labelled

A published report is committed unchanged under `docs/benchmarks/`, named for the date
and the machine that produced it. The first two are
`2026-10-28-windows-wsl2-sqlite.json` and `2026-10-28-github-postgres17.json`. The
plan had named them for 2026-10-27 and a Mac; the measurements were taken on 2026-10-28
and the SQLite one on a Windows PC under WSL2, so a name that said Mac would have been
false.

`docs/performance.md` quotes a report in its Results section, one subsection per
report, and names beside it the machine, operating system, Go version, commit, backend
and trial count. A table there copies the report without rounding and the line above
it cites the file. Prose may round; a table does not.

### The rule, and the test that holds it

The rule is that every published figure appears in a committed JSON report.
`TestBenchmarkDocsMatchTheirReports` (`cmd/twinwright/benchdocs_test.go`) holds the part
of it that is cheap to hold exactly. It decodes every `docs/benchmarks/*.json` as a
`perf.Report`, strictly, so a file that is not a perf report (a bench report, an empty
object, a report with an unknown field or data after it) fails. It reads the Results
section of `docs/performance.md`, finds each table headed `Runs/s median`, and requires
every row's minimum, median and maximum to equal, as numbers, the row of the report the
table cites for the same worker count. It fails when there is no report, no Results
section, no quoted median, a citation of a file that is not committed, or a row whose
worker count the report lacks. It runs with `go test ./...` and needs nothing outside
the repository.

It does not read the recovery, replay and crash matrix tables or the prose. They were
checked against the same two reports by a script when they were written: every table
cell, the machine details against the runner's `runner.txt` and the measurer's notes,
and every numeric token in the new sections against the reports. That script is not part of
the repository, and it ran once. It also cannot tell a report the command wrote from
one someone edited, because nothing signs a report.

### The CI job

The `perf` job of `.github/workflows/integration.yml` runs only on `workflow_dispatch`.
It starts a `postgres:17` service container on `ubuntu-latest`, builds the binary,
writes the runner's CPU count, CPU model, memory, Go version and PostgreSQL server
version to `runner.txt`, runs `twinwright perf --db "$DSN" --trials 3 --out perf.json`,
and uploads both files as the artifact `perf-postgres17`. It fails if perf exits
non-zero. It is dispatch-only because it takes minutes and the figures mean something
only when someone intends to record them. Running it on every push or pull request
would spend runner time on numbers nobody reads and invite comparing two runs from
different shared machines. Integration run 40743500240 produced the committed
PostgreSQL report.

## What is proven

By tests that run on every `go test ./...`: the report has every section and no `dsn`
(`TestPerfReportShape`, `TestPerfCLIWritesTheReport`); a one second lease gives a
recovery median between one and three seconds and no recovery earlier than the lease
(`TestPerfRecoveryIsBoundedByTheLease`); the throughput table in `docs/performance.md`
equals its report row for row (`TestBenchmarkDocsMatchTheirReports`). By hand, when the
test was written, each of these made it fail: a rounded median, a changed maximum, a
median swapped between two tables, a row for a worker count the report lacks, a table
citing a file that is not committed, a missing or renamed Results section, and a JSON
file that is not a perf report.

By the two reports, each one run on one machine: replay verified 20 of 20 runs on
both backends, the duplicate-charge crash matrix ran 25 cases with 0 violations on
both, and no recovery duplicated a committed mutation. The figures are in
`docs/performance.md`.

The first SQLite report is unfavourable and is published as it is. Throughput did not
scale with workers on that machine: two workers were slower than one and eight were
faster than four. The SQLite store uses one connection (`SetMaxOpenConns(1)` in
`internal/store/store.go`), which serializes its writes. The cause of the eight-worker
figure was not investigated, and one machine with three trials is not enough to
explain it. PostgreSQL throughput rose with workers on the runner.

## Limits

- **The two reports are not a SQLite versus PostgreSQL comparison.** The machines
  differ and so do the operating system layers: a desktop under WSL2 that was running
  other software, and a shared virtual machine. A figure from one says nothing about
  the other.
- **One run per report.** How much a figure varies between runs of the same job, or
  between days, was not measured. GitHub runner figures come from a shared machine.
- **Recovery latency is in process, on a real clock.** The recovering worker polls every
  100 ms, so the figure includes up to one poll interval. A worker that died earlier in
  its renewal period would be recovered sooner; that was not measured. The real-process
  crash evidence remains `TestRealProcessDeathIsRecovered` (ADR 0029), which shows the
  run is recovered and times nothing.
- **Trials on PostgreSQL share one database.** Each starts only when the queue is idle,
  so later trials run against the rows earlier ones left. The effect was not measured.
  On SQLite each throughput trial and each crash matrix case has a file of its own.
- **The environment details outside the report are not held by a test.** See the
  environment block above.
- **Not measured:** real-process recovery latency; multi-process or multi-host fleets
  (every worker in a trial is a goroutine of one process); network latency between the
  workers and the database (both reports ran them on one machine); live model providers
  (the agents are scripted); memory and CPU usage; long-running stability; anything on
  macOS; PostgreSQL on the local machine; SQLite on the runner.

## Rejected

**Typing figures into the document.** It is quick, and the figure then has no machine,
build or backend next to it that anyone can check.

**Running `perf` on every push.** The workflow comment gives the reason: runner time
spent on numbers nobody reads, and an invitation to compare two runs from different
shared machines. It stays on demand.

**A threshold on speed.** It would fail on the noise of a shared runner, and it would
turn a measurement into a target. The command's findings are the invariants that
have a right answer: no duplicated mutation, no crash matrix violation.

**One table comparing the backends.** The two reports differ in machine as well as
backend, so a joint table would invite a conclusion they do not support.

## Consequences

- A new published figure needs a new committed report, and the throughput table that
  quotes it is held to that report by the test. A report keeps the name of the date and
  machine that produced it; changing the code later does not change it.
- `docs/performance.md` carries two kinds of figure, and says which came from which
  tool. Its Observations section still says that SQLite throughput cannot improve by
  adding workers. That sentence is left as it was written, because it records what was
  believed then, and the Results section says the first report does not support it as
  written.
- The `perf` job costs runner time only when someone dispatches it.
- The report carries `schema_version`, 1 today. The test refuses a committed report
  whose version is 0 or newer than the build writes.

## Corrections

- 2026-10-28: "The database address is never recorded" claimed that an error from `perf.Run` has the DSN and its password removed, and cited two tests for it that pass with the scrubber removed. That held only for a DSN that parses: for a PostgreSQL password with an unencoded `/`, `#` or `?` the error quoted a piece of the password, and `redact.URL` kept the userinfo of such a string. The scrubber now replaces the text of an error about a DSN that does not parse cleanly, and the cited tests now use such DSNs.
