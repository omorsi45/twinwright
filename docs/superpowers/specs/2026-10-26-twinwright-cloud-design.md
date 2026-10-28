# Twinwright Cloud design

Date: 2026-10-26. Status: approved (the owner delegated every open choice to the
recommended option; calls made on that basis are marked "our call").

## Intent

Twinwright already simulates stateful worlds, records a durable ledger, replays,
forks, injects chaos, enforces principal authorization and runs a fenced
multi-worker fleet. Its own README admits the gaps that matter to an engineer
evaluating it: no real model has ever run through it, nothing gates a
regression, traces have never reached a real collector, crash recovery is tested
at one execution boundary, and the published benchmarks cover one laptop.

Twinwright Cloud is the release that closes those gaps with evidence. The
audience is engineers at AI infrastructure companies reading the repository to
judge whether its reliability claims are true. Success is a claim list in which
every line names the test or CI job that proves it, plus a demo a reader can run
with one command.

## Goals

1. Run real models (at least one real provider) against isolated stateful
   worlds, record what they did, and reconstruct the run offline with no model
   call.
2. Recover from worker crashes at every documented execution boundary without
   duplicating a logical world mutation, and prove it with deterministic
   injection and with real `SIGKILL`ed processes.
3. Compare a candidate agent (version, model, or execution condition) against an
   approved baseline and fail CI on a regression, with machine-readable reports.
4. Deliver traces to a real OpenTelemetry collector and prove they are ingested
   and queryable; expose queue, worker and failure metrics a real Prometheus can
   scrape; correlate both back to ledger events.
5. Let a developer test their own agent, written in any language, through a
   versioned protocol, inside a documented trust boundary.
6. Publish reproducible correctness and performance benchmarks with captured
   configuration and runtime metadata, including unfavourable results.
7. One-command demos: a hermetic one (no network, no Docker) and a full-stack one
   (PostgreSQL, worker fleet, collector, trace backend, Prometheus).

## Non-goals

- A hosted, multi-tenant service. "Cloud" names the self-hosted platform: a
  worker fleet over PostgreSQL with a regression gate and an observability
  stack. No accounts, no tenants, no web UI (our call; rationale under
  Approaches).
- An HTTP control-plane API for submitting runs. The PostgreSQL queue is already
  the integration point, and a network API adds an authenticated attack surface
  the project does not need for any goal above. Deferred, recorded in an ADR.
- A public Go library (`pkg/`). The stable contracts are the CLI, the agent
  protocol and the versioned JSON report schemas.
- Kubernetes, message brokers, sharding, multi-region.
- Exactly-once delivery across external services. Never claimed.

## Anti-goals (would make the release a failure even if it "works")

- A README feature that no test or CI job exercises.
- A benchmark number with no recorded machine, commit, configuration and method,
  or a favourable number reported while an unfavourable one is omitted.
- A gate that cannot fail, a crash test that cannot detect a duplicate, a trace
  test that only checks a fake server.
- Infrastructure added because it looks impressive rather than because a stated
  goal needs it.
- Rewriting working subsystems (ledger, fencing, replay, chaos, authz) instead of
  extending them.

## Constraints

- Go 1.27.2 (`go.mod`). New module dependencies need a written reason in the
  milestone's ADR; prefer none. Milestone 1 adds none.
- `go test ./...` stays hermetic: no network, API key, Docker or PostgreSQL.
  External dependencies are opt-in by environment variable, following the
  existing `TWINWRIGHT_TEST_POSTGRES_DSN` and `TWINWRIGHT_TEST_DOCKER=1` pattern.
- `make verify` and `make test-race` must pass at every commit boundary.
  `make verify-full` and the Integration workflow must pass at every milestone
  boundary.
- Invariants in `AGENTS.md` are preserved: `world_id` isolation; call-ID
  idempotency with different-argument reuse refused; atomic local effects and
  ledger events; authorization in code inside the tool transaction; replay and
  inspection never mutate the source; fenced durable writes; at-least-once
  delivery; the ledger records the agent trajectory, never worker identity;
  shadow mode is observe-only.
- Any change to a guarantee gets an ADR in `docs/adr/` and adversarial tests.
- Documentation states only what a test or CI job proves, and says "not
  measured" or "not verified" otherwise. No em dashes in written text.
- Commits explain why. No assistant attribution in commits, PRs or docs.
- Local development host: Apple M1, 8 GB, macOS 27, no Docker. PostgreSQL,
  container, collector and trace-backend evidence comes from GitHub Actions.

## Current state (verified 2026-10-26 on `main` at `0a61188`)

Baseline: `make check`, `go test ./...`, `go test -race ./...`,
`scripts/e2e.sh` and `twinwright demo` all pass locally; CI and Integration
(PostgreSQL 16 and 17, Docker) are green on `main`.

Verified defects and gaps that shape the milestones:

- Lease expiry and queue availability are compared as RFC 3339 text in SQL
  (`internal/store/queue.go` ClaimRun, `internal/store/lease.go`
  RenewRunLease) while `GuardFenceTx` parses in Go. `RFC3339Nano` trims trailing
  zeros, so lexical order disagrees with time order inside a second: an expired
  lease can be renewed, and an expired lease can be missed by a claim.
- A run whose worker dies is taken over indefinitely: `ClaimRun` increments
  `attempts` but never filters on it, and `MaxAttempts` is checked only when
  `Execute` returns an error.
- `Outcome.Takeover` is `attempts > 1`, so a requeue after an ordinary error is
  counted as a takeover.
- Crash tests cut only inside a provider call. In the billing fixture the refund
  commits after the last tested crash point, so "exactly one refund after a
  crash" passes trivially. The only post-mutation crash (`flagship.yaml` crash at
  turn 9) runs in `demo`, not in `go test`.
- A provider that reuses a call ID in a later turn has that call silently
  skipped by the runner.
- `twinwright resume` executes without a fence, so it can race a fleet worker on
  the same run.
- `bench`, `compare` and `run` exit 0 regardless of outcome; bench reports carry
  no seed, git revision, Go version, OS or CPU; the suite digest ignores fixture
  contents.
- Provider adapters are tested only against `httptest` servers; no real model
  run, no recorded wire exchange; replay never exercises adapter translation;
  runs that saw a non-injected provider error are unreplayable; 429 and 5xx are
  not retried.
- OTLP is hand-encoded JSON, verified only against `httptest`; the shipped
  collector config has only a `debug` exporter; spans drop event sequence numbers;
  `twinwright_worker_lease_renewals_total` is never incremented.
- Agent code cannot be plugged in except by writing an OpenAI-compatible server.

## Approaches considered

1. **Harden and prove the existing platform (chosen, our call).** Fix the
   verified defects, extend tests to every execution boundary, add recording,
   gating, collector and agent-protocol capabilities on the existing packages,
   and add only the infrastructure a test needs (collector, trace backend,
   Prometheus and a local model server, in Compose and CI). Trades breadth of
   new surface for depth of evidence, which is what the audience is judging.
2. **A literal hosted control plane.** HTTP API server, authentication, tenants,
   run submission, dashboards. Rejected: it duplicates the PostgreSQL queue,
   creates the largest new attack surface in the project, and serves none of the
   seven goals better than the CLI plus queue. It is the "infrastructure for
   appearance" the brief prohibits.
3. **Re-platform on external engines.** Temporal for durable execution, a broker
   for the queue, the OpenTelemetry SDK for traces. Rejected: it replaces the
   verified ledger and fencing with external semantics that the project would
   then have to re-verify, adds heavy dependencies, and gains no evidence. The
   OTLP encoder stays hand-written because traces are projected from the ledger
   after the fact, which is not what the SDK is built for; its conformance is
   proven against a real collector instead.

Sub-choices, all our call:

- **Real model in CI:** a local Ollama server running a small tool-calling
  model through the existing `openai-compatible` adapter. Real inference with no
  paid key, runnable on a GitHub runner and on the development Mac. Paid OpenAI
  and Anthropic runs are added as opt-in tests gated on their keys.
- **Crash injection:** both deterministic in-process hooks (fast, exhaustive,
  run in `go test`) and real `SIGKILL` of a worker process (truthful about what
  survives a process death, including an open SQLite or PostgreSQL transaction).
- **Trace backend:** collector forwarding OTLP to Jaeger, whose query API gives
  a test a way to fetch a trace by ID.

## Milestones

Ordered by dependency. Milestone 1 is its own branch. At the owner's request
(2026-10-27), milestones 2 to 7 below are delivered as one final milestone on
one stacked branch, `cloud/m2-platform`, with one plan, per-task reviews and
one whole-branch review. Their sections stay as the requirements; the section
"Final milestone: scope decisions" records what was cut to finish faster.

| # | Milestone | Depends on | Why this position |
|---|---|---|---|
| 1 | Reliability core: crash boundaries, delivery bounds, timestamp ordering | none | Every later claim (gate, benchmarks, demo) rests on recovery being correct |
| 2 | Regression gate and reproducible reports | 1 | Hermetic, highest product value, and it gates every later milestone in CI |
| 3 | Real provider execution, recording and offline reconstruction | 2 | Needs the gate to compare a real model against a baseline |
| 4 | Observability against a real collector and Prometheus | 1 | Needs correct takeover semantics for its metrics |
| 5 | Bring-your-own-agent protocol and isolation boundary | 3 | Reuses the recording and replay path built for providers |
| 6 | Reproducible reliability and performance benchmarks | 1, 4 | Uses the crash harness and the metrics |
| 7 | Twinwright Cloud packaging: full-stack demo and documentation | all | Documents only what earlier milestones proved |

### Milestone 1: reliability core

Outcome: a worker crash at any documented execution boundary is recovered by
another worker, with no duplicated logical world mutation, a ledger that still
replays, checkpoints that still reconstruct, and a stale worker that cannot
commit. Proven exhaustively in-process and by real process death.

Decided:

1. **Canonical sortable timestamps for coordination columns.** One formatter,
   UTC with exactly nine fractional digits (`2006-01-02T15:04:05.000000000Z`),
   used for every value written to `run_leases`, `work_queue` and `workers`. It is
   still RFC 3339, so existing parsers read it. A new migration rewrites existing
   rows in those tables into the canonical form, so lexical comparison in SQL
   equals time comparison. Ledger `events` timestamps are not touched, because
   replay compares them byte for byte.
2. **Delivery bound.** A delivery that never reports back (a crash) counts toward
   `MaxAttempts`. When a claim takes over a run whose attempts already exceed
   `MaxAttempts`, the worker does not execute it. If the run is already
   `completed` (it died at `after-completion`), the worker finishes the queue
   entry normally. Otherwise, under its fresh lease and in one fenced
   transaction, it sets the run's status to `failed`, appends an `error` ledger
   event `{"kind":"delivery_limit","deliveries":N,"max_attempts":M}` (no worker
   identity: it records how execution ended, not who owned it), and marks the
   queue entry failed with the same reason. `Outcome.Takeover` becomes true only
   when the claim displaced an expired lease (our call on all of the above).
3. **Crash points.** A small `internal/crashpoint` package defines named
   boundaries and a hook type. `agent.Runner` and `dispatch.Dispatcher` gain an
   optional hook field, nil in production. A hook that returns an error is a
   simulated process death: the caller returns immediately and writes nothing
   further. The boundaries:

   | Point | Durable state at the moment of death |
   |---|---|
   | `before-model-request` | previous step fully committed; no request for this turn |
   | `after-model-request` | `model.request` committed; no response |
   | `after-model-response` | turn committed (response, step, transcript); its tool calls not dispatched |
   | `before-tool-commit` | inside the tool transaction with every write staged, not committed |
   | `after-tool-commit` | tool transaction committed (effect, events, saved result, transcript) |
   | `before-completion` | final answer committed; run not marked completed |
   | `after-completion` | run completed; queue entry not finished (simulated by not calling `FinishRun`) |

4. **Crash matrix harness.** A reusable `internal/crashmatrix` package runs one
   scenario once without a crash to count every occurrence of every point, then
   for each (point, occurrence) crashes a first delivery there, expires its
   lease, lets a second worker take over, and checks the invariants: the run
   completes under a strictly higher fence and is reported as a takeover; the
   scenario judge passes; agent-attributed state mutations equal the reference
   run's and no call ID has two; per-table world row counts equal the reference;
   `replay.Verify` passes; the latest checkpoint at or before the crash and the
   final checkpoint both reconstruct; model requests
   equal responses; and the dead worker's fence is refused both for a new
   mutating call and for an already-committed call ID. Scenarios:
   `duplicate-charge` (billing), `company-incident` (company world), and
   `ambiguous-commit` with `examples/chaos/ambiguous-commit.yaml` and the safe
   fixture. Runs on SQLite in `go test`, and on PostgreSQL when
   `TWINWRIGHT_TEST_POSTGRES_DSN` is set.
5. **Real process death.** A unix-only test re-executes the test binary as a
   worker process that sends itself `SIGKILL` at a chosen crash point, including
   inside the open tool transaction and right after the refund commits. A second
   worker takes over against the same SQLite file (and PostgreSQL when
   configured) and the same invariants are checked.
6. **Call-ID reuse refused.** A provider turn that reuses a call ID already used
   in the run, or repeats one within the turn, fails the turn with an explicit
   error instead of being skipped.
7. **Local execution holds a fenced lease.** `twinwright run` (without
   `--enqueue`) and `twinwright resume` execute through one local executor that
   acquires a lease on the run (owner `local-<host>-<pid>`), renews it while
   executing, passes its fence to the runner and dispatcher, and releases it at
   the end. A run leased by a live fleet worker is refused with `ErrLeaseHeld`,
   and a fleet worker cannot claim a run while the local lease is live, because
   `ClaimRun` already excludes runs under another owner's unexpired lease. This
   closes the race a queue-state check would leave open, including an operator
   enqueuing a run that is executing locally (our call). Command output and exit
   codes are unchanged.
8. **Bench uses the harness.** The distributed bench mode delegates to
   `crashmatrix`, gains optional `crash_point` and `crash_operation` case fields
   (an integer `crash_at` keeps meaning `after-model-request` at that turn), and
   the standard suite gains a case that crashes right after the refund commits.
   The flagship suite's crash case runs in `go test`.
9. **Docs.** ADR 0029 records the boundaries, the delivery bound, the timestamp
   form and a precise statement of what is idempotent and under which
   conditions; it corrects ADR 0020's overstated claims. `docs/distributed.md`
   gains the boundary table with the test that proves each row.

Idempotency statement to publish (our wording, to be kept true by tests): a
world mutation made by a tool call commits at most once per (run, call ID),
because the effect, its ledger events, its saved result and its transcript entry
commit in one transaction that first proves lease ownership. Model calls are
at-least-once: a crash before a response is recorded repeats the provider call,
and its cost. A retry with a new call ID is a new logical action and is not
deduplicated; that is the behaviour the ambiguous-commit scenario measures.
Effects outside the world store (provider spend, logs, metrics) are not fenced.

Left to the builder: internal names beyond those above, the occurrence-counting
mechanism inside the hook, how the matrix test shards its cases, and test helper
layout.

### Milestone 2: regression gate and reproducible reports

- Bench report schema version 2: `schema_version`, `twinwright_version`,
  `git_revision` (from build info), `go_version`, `goos`, `goarch`, `num_cpu`,
  `seed`, `started_at`, and a suite digest that covers every referenced fixture
  file's bytes. Version 1 reports still load for comparison.
- A per-case behavioural fingerprint: the tool-call trajectory (operation and
  canonical argument digest with run-derived IDs normalised), mutation counts per
  table, assertion verdicts, security outcome, and a final-state digest over
  normalised rows.
- `twinwright gate --baseline <report> --candidate <report>` (or `bench
  --baseline`) exits non-zero on a regression: a case that passed now fails, a
  negative control stops being detected, a safety or authorization outcome
  worsens, a new forbidden mutation appears, or (configurable) a trajectory
  diverges. Output: JSON gate report, a Markdown summary for the CI step summary,
  and JUnit XML.
- Nondeterministic candidates (our call): schema v2 carries `trials` per case
  and per-case `passes`, so the stable contract does not need a v3 for live
  models. The gate compares pass rates: a regression is a candidate pass rate
  below the baseline's by more than `--tolerance` (default 0). Deterministic
  agents (scripted fixtures and recorded cassettes) run one trial and therefore
  compare exactly. CI gates only deterministic and replayed-cassette reports;
  live-model reports are produced and published but gate only when an operator
  passes an explicit tolerance and trial count.
- `run --fail-on-eval` exits non-zero when evaluation fails (opt-in, so existing
  scripts keep working).
- An approved baseline committed under `examples/bench/baselines/`, a CI job that
  gates on it, and a test plus demo step that introduce a deliberate regression
  (a degraded scripted fixture) and assert the gate names it.

### Milestone 3: real provider execution, recording and offline reconstruction

- A recording HTTP transport around provider clients writes a redacted cassette
  of wire exchanges; a `cassette` mode serves them back through the real adapter,
  so offline reconstruction exercises adapter translation, not only the ledger.
  Strict matching: a request that does not match the next recorded exchange is a
  divergence error.
- Additive fields on recorded turns: response ID, server-reported model, finish
  reason, HTTP status.
- Bounded, recorded retries for 429 and 5xx honouring `Retry-After`; an explicit
  error for an oversized body instead of a silent cut; SIGINT and SIGTERM
  handling for `run`; `--max-output-tokens`.
- Runs that met a provider error and were resumed become replayable.
- Redaction extends to transport error text (URL query and userinfo) and to the
  configured secret anywhere in recorded provider output.
- An Integration job runs a real small model under Ollama against a scripted
  world, records it, then reconstructs it offline with networking disabled for
  the replay step. Opt-in tests for OpenAI and Anthropic run when their keys are
  set. A local demonstration on the development Mac is recorded with the model,
  the machine and the actual outcome, pass or fail.

### Milestone 4: observability

- Compose and CI stack: collector (OTLP/HTTP in, `otlp` to Jaeger plus `debug`),
  Jaeger, Prometheus.
- An Integration test exports a run's trace through the CLI to the collector,
  fetches it from Jaeger's query API by trace ID, and asserts span count, names
  and attributes.
- Spans carry ledger sequence numbers; the CLI prints the trace ID on delivery;
  worker logs carry the trace ID; workers can export a run's trace on completion.
- Metrics fixes: renewals counted, zero-valued series emitted, correct label
  names, run-duration histogram, oldest-runnable age, worker staleness; fleet-wide
  gauges served once rather than summed per worker. A real Prometheus scrapes a
  worker in CI and a test queries it.

### Milestone 5: bring-your-own-agent

- An `exec` provider speaks a versioned JSON-lines protocol over stdio with a
  user-supplied agent process: Twinwright sends the task, history and exposed
  operations; the agent returns one decision. It slots into the existing Provider
  interface, so recording, replay, forks and counterfactuals work unchanged.
- Protocol documented with JSON Schema; a Python reference agent in `examples/`.
- Trust boundary: the agent process holds no database credentials and can only
  propose tool calls, which are authorized and executed by the dispatcher. It runs
  with the operator's privileges unless the optional container mode is used
  (read-only root, all capabilities dropped, no-new-privileges, non-root user,
  resource limits); limitations documented, including that network access for the
  agent's own model calls is not restricted by Twinwright.

### Milestone 6: reproducible benchmarks

- `twinwright perf` (or a script over it) produces JSON with full metadata: fleet
  throughput with 1, 2, 4 and 8 workers on SQLite and PostgreSQL, crash recovery
  latency against lease TTL with real processes, replay consistency over N
  recorded runs, and crash-matrix duplicate-effect counts. Fixed seeds and
  workloads; repeated trials with spread reported.
- Results published with the machine that produced them: the development Mac for
  SQLite and GitHub runners for PostgreSQL, labelled as shared-runner figures.

### Milestone 7: packaging

- `make demo` (hermetic) and `make demo-stack` (Compose: PostgreSQL, workers,
  collector, Jaeger, Prometheus, optional Ollama) as the one-command demos.
- README rewritten as Twinwright Cloud with a quick start and a claims table
  linking each claim to its test or CI job; an architecture document; security,
  trust-boundary and limitations documentation; benchmark methodology and
  results; CHANGELOG and a release tag decision left to the owner.

## Final milestone: scope decisions (2026-10-27)

Milestones 2 to 7 are built as one branch. These calls narrow them so the
release finishes sooner without dropping any goal or success criterion; all are
our call.

- **Gate fingerprint:** the per-case fingerprint is the tool trajectory
  (operation and status per call), the agent's committed mutation count, failed
  checks, unsafe-retry and successful-violation counts. The normalised
  final-state digest is cut; replay and assertions already cover state.
- **Trials:** `trials` above 1 is supported for local-mode cases only; distributed
  and counterfactual cases refuse it.
- **Deliberate regression:** a scripted model `fixture-regressed-v1` wraps any
  scripted fixture and answers instead of making the run's first mutating call,
  so every scenario that must write regresses the same visible way.
- **Provider resilience:** 429, 5xx and transport failures become recorded
  interruptions retried within the existing two-attempt budget, waiting the
  server's `Retry-After` (capped at 30 seconds) or one second. The attempt count
  is unchanged.
- **Error-run replay is cut.** Runs that hit the delivery limit or a non-retried
  provider error stay unreplayable and documented as such; transient provider
  failures no longer end a turn, which removes the common case.
- **Cassettes:** recorded as JSON lines; offline reconstruction is
  `twinwright replay <run> --cassette <file>`, which drives the real adapter from
  the cassette and must reproduce the ledger exactly.
- **Real model:** a pinned Ollama image with a small tool-calling model (chosen
  when the model is pulled and recorded with its digest) runs in an Integration
  job and once on the development Mac. The run's evaluation outcome is reported
  as measured, pass or fail; the job gates on recording and offline
  reconstruction, not on the model's competence.
- **Agent protocol:** the `exec` provider starts the agent process once per turn
  and exchanges one JSON request and one JSON response. The child gets a minimal
  environment (PATH, HOME, and only variables named with `--agent-env`). The
  command is not persisted; resume and workers take it again.
- **Agent isolation:** `--agent-container <image>` runs the same protocol under
  `docker run --rm -i` with a read-only root, all capabilities dropped,
  no-new-privileges, a non-root user, resource limits and no network unless
  `--agent-network` names one.
- **Benchmarks:** `twinwright perf` measures throughput with in-process workers,
  recovery latency with an in-process crash on a real clock (the real-process
  crash evidence stays the milestone 1 test), replay consistency and the crash
  matrix. SQLite figures come from the development Mac and PostgreSQL figures
  from a GitHub runner, each labelled with its machine.
- **Fleet gauges:** database-derived gauges stay served by every worker; the docs
  say to scrape one worker or aggregate with `max`. Re-architecting them is cut.
