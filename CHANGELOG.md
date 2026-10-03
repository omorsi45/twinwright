# Changelog

Notable changes, newest first. Dates are the merge date. Versioning follows
[Semantic Versioning](https://semver.org); while the major version is 0 the
runtime schema and CLI surface may still change, and each entry says when they
do.

The database schema is versioned separately and recorded in `schema_migrations`.
`twinwright version` reports the schema version a build writes. Opening a
database written by a newer build is refused rather than downgraded.

## Unreleased

### Added

- **A documented security review, gated by a test.** `SECURITY.md` was the policy
  and there was no record of anyone having looked: nothing under `docs/` matching
  secur, audit, review or threat, and no ADR. `docs/security-review.md` is the pass
  over seven surfaces - the authorization boundary and its eight deny reasons,
  prompt injection through world content, secret redaction at write time and read
  time, the container surface, the shadow connector's observe-only guarantee, SQL
  construction, and what is deliberately not covered - with the file and the test
  that hold each claim, and with the places where no test holds one written down as
  limits rather than left out.
  `cmd/twinwright/security_review_test.go` requires every cited file and test to
  exist and every surface to be present, and fails outright if it parses nothing,
  so the document fails the build when it drifts.
- **Two claims `SECURITY.md` already made are now tested.** "Provider errors are
  recorded without credentials" is a claim about the ledger and only the provider's
  return values were asserted, so `TestProviderErrorReachesTheLedgerWithoutTheKey`
  now runs a real provider against a server that echoes the configured key into a
  401 and asserts no event payload and no stored transcript carries it. The one
  interpolated SQL identifier is documented as validated first and nothing
  exercised that, so `TestSearchPathSchemaRejectsAnythingButAPlainIdentifier`
  refuses seven hostile `search_path` values with legal controls beside them. A
  third test covers an `env_file` traversal written in the unnormalised form the
  existing cases had already normalised away. See ADR 0028.
- **Per-attempt provider deadlines, and truncated completions that stop being
  decisions.** Every provider built an `http.Client` with a flat, hard-coded,
  uncancellable 90 second timeout, and nothing in the repository read
  `finish_reason`, `stop_reason` or the Responses API's `incomplete_details`. So a
  hung call took 90 seconds off the run and left nothing in the ledger, and a
  completion the provider cut off at its token ceiling was consumed as a complete
  answer: a tool call cut mid-arguments still decodes into a map, and the runtime
  would dispatch an action built from half a serialisation. The deadline is now per
  attempt and carried on the request context, configurable with
  `--provider-timeout` on every command that can reach a provider and refused if
  non-positive. Each provider reads its own surface's truncation field and returns
  before parsing any output item. Either failure is retried exactly once and every
  attempt is recorded as a `provider.interrupted` event carrying the kind, the
  attempt number, the vendor's verbatim reason and the configured deadline; a run
  where every attempt is interrupted fails with the existing `error` event of kind
  `provider`. The record lives on the turn, so a run that recovered still replays:
  a replayed run appends the same event from the recorded turn instead of needing
  the provider to fail again, which is also why the event is in replay's semantic
  list rather than ignored by it. `AnthropicProvider.MaxOutputTokens` makes the
  ceiling a truncated completion hit configurable, since raising it is the
  documented remedy. See ADR 0027.
- **Shadow comparison depth.** The comparison was an exact match on marshalled
  arguments, so a proposed refund of 500 against an observed refund of 5905 on the
  same charge was reported as two unrelated entries, one in each only-list, and
  the reader had to notice they were the same charge. Three classifiers report it
  instead: `argument_divergences` pairs the leftover actions that address the same
  resource with the same operation and names the fields that differ with both
  values; `timing` reports the pairs the two streams sequence differently; and
  `policy` screens each proposed action against a principal policy given with
  `shadow --policy` and lists what it would refuse, with the permission, the deny
  reason and the call number. A refusal stands even when the observed stream
  contains the same action, flagged `also_observed`, because a human with other
  permissions doing something is not evidence that this principal may. Resource
  identity comes from the arguments that name the resource, not from the operation
  name and not from a call's position, both of which stop identifying anything
  once a call site is added. Elapsed time is reported as not compared rather than
  as zero: a local simulation has no wall clock comparable to a recorded stream.
  Shadow mode stays observe-only, the output stays deterministic and sorted, and
  it still declares no winner. See ADR 0026.
- **`authz.Screen`.** The policy decision for one call at a given call number,
  without consuming one. `authz.Decide` is now `Screen` plus the two things only a
  live run supplies, the stored policy and the next call number, so a shadow
  verdict and an enforced one cannot drift apart.

## v0.1.0 - 2026-09-30

First tagged release. Everything below shipped before the tag; the sections are
grouped rather than dated individually.

### Added

- **`twinwright demo`.** One command that walks the whole runtime over the
  flagship incident in eleven steps: compile the four-service world, run it under
  chaos, inspect, trace, evaluate assertions, replay, fork and compare, crash a
  worker and watch another take over, fail the same incident on purpose, explain
  that failure with a counterfactual, and refuse a prompt injection and an
  over-privileged request. Each step executes through the CLI's own entry point,
  so it runs the documented interface rather than a parallel copy, and reads its
  evidence back out of the run. The walkthrough stops at the first step that
  cannot produce its evidence, and CI runs it, so a step that stops being true
  fails the build.
- **`event_follows` assertion operator.** Anchors on the first matching event and
  requires a later match, so existence is part of the claim. `event_order`
  forbids an event from happening early and is therefore satisfied when it never
  happens at all, which is the right operator for "do not comment before reading
  the ticket" and the wrong one for "reconciled after the loss".
- **Negative controls in Bench suites.** `expect: fail` declares a case that is
  supposed to fail, and a misspelling is refused rather than defaulted. A control
  that starts passing is reported as an undetected control instead of a green
  case.
- **Flagship distributed bench suite.** `examples/bench/flagship.yaml` crashes the
  first worker at three points of a sixteen-call trajectory on the four-service
  company world and requires a takeover, a duplicate delivery and a refused stale
  commit on each. The three shipped distributed cases all used the single-service
  billing world, so the headline claim had never been measured on the incident the
  README leads with.
- **Unsafe company fixture.** Selected by `--model fixture-unsafe-v1`, as the
  billing world already did. It differs from the safe fixture by one decision,
  whether a lost write response is taken as proof the write landed, which gives
  counterfactual analysis a genuinely failing parent on the flagship scenario.
- **Distributed runtime documentation.** `docs/distributed.md` carries the
  topology, the crash-and-takeover sequence and the lease lifecycle. Every
  database identifier in those diagrams is checked against the migration DDL, so
  a renamed table breaks the diagram that names it. (ADR 0020)
- **Shadow observation connectors.** A `Connector` boundary decodes a recorded
  external format into observations, so shadow evaluation no longer requires
  hand-transforming an export into Twinwright's own shape first. Three
  implementations ship: `file`/`jsonl` (native), `audit_log` (sanitized audit
  export) and `recorded_http` (captured HTTP interactions), selected by a
  config's `source.type`. Decoding is a pure function of bytes, so no connector
  opens a socket or holds a credential. Confinement under the examples root is
  enforced in the loader as well as at config parse time, sources are capped at
  8 MiB, and unknown fields are rejected rather than silently dropped. An attempt
  that did not take effect (a denied audit entry, a 4xx or 5xx response) is not
  counted as an observed action. No connector has been tested against a live
  external system; shadow output carries `live_external: false`. (ADR 0022)

### Fixed

- **Bench reported a safety figure that covered fewer cases than it claimed.**
  A case that errored and measured nothing was counted as a compliance failure in
  every dimension it declared, and the two deliberate negative controls were
  counted as agent failures, which is how the standard suite reported 25% safety
  compliance while no agent had misbehaved. Errored cases and declared controls
  now leave the rate denominators, the report names both counts, and the footer
  states how many cases a percentage covers. (ADR 0025)
- **A rate over an empty denominator printed as `0.0%`.** A suite declaring only
  some dimensions reported `Safety Compliance 0.0%`, which reads as the opposite
  of the truth: the suite contained no safety case. The text report now labels the
  absence, and the JSON keeps the numeric field beside a `measured` map so a
  machine consumer can tell zero-of-zero from zero-of-many. (ADR 0025)
- **The duplicate-effect rate counted refund rows, not the agent's refunds.** That
  assumes the agent is the only writer, so the one case built to exercise a
  concurrent writer was scored as the defect it exists to rule out. The rate now
  counts refunds the agent itself committed, paired from its own requests and
  responses in the ledger; a chaos actor's write is recorded as
  `chaos.actor_mutation` and is not a tool call, so the separation is exact rather
  than heuristic. (ADR 0025)
- **A concurrent partial refund made the flagship fixture stop rather than
  reconcile.** Billing refuses a refund exceeding what a charge still owes, so an
  amount computed from an invoice read earlier is rejected once another writer has
  moved money. The fixture re-reads the charge and refunds the remainder, so the
  customer ends at exactly the charge amount and never above it.
- **A refusal was read as proof the money was already back.** That holds for a
  fully refunded charge and fails for a partly refunded one, leaving a customer
  short while the agent reported the incident resolved, with nothing in its own
  transcript to show it. The refusal now triggers one fresh read and a refund of
  what is still outstanding, and a read that contradicts the writer stops the run
  instead of reporting a resolution.
- **An unbounded stale-read rule poisoned every later read.** `after_calls` with
  no `times` models a replica that never catches up, which is a different fault
  from the single stale observation the test was named for.
- **Shadow comparison is now deterministic.** `Compare` built its matched and
  residual lists by ranging over maps, so Go's randomised iteration order decided
  the order of a report meant to serve as evidence: two comparisons over
  byte-identical input disagreed, which made a report impossible to diff between
  runs or commit as a fixture. Output is sorted by operation and then canonical
  arguments.
- **Superseded ADRs now say so.** ADR 0018 still read as current after ADR 0020
  replaced its design and removed the `twinwright lease` command, so a reader
  landing there first would implement a lease store that no longer exists and
  cannot enforce fencing. ADR 0017 now credits the two ADRs that completed it,
  and the README capability list no longer advertises the distributed runtime as
  deferred two bullets above the ones describing it as shipped.

- **PostgreSQL storage backend.** `store.OpenDSN` accepts a `postgres://` URL or
  a SQLite path. Runtime SQL is written once with `?` placeholders and translated
  to `$N` by a `database/sql` driver wrapping pgx. SQLite remains the default for
  local development, deterministic examples and replay reconstruction.
  (ADR 0019, schema version 1)
- **Versioned migrations.** Schema state lives in `schema_migrations` instead of
  being inferred from `PRAGMA` probes. Each migration commits with its own
  bookkeeping row, so a failure leaves neither a partial schema nor a false
  record of success. Databases written before migration tracking are adopted in
  place.
- **Multi-worker runtime.** `twinwright run --enqueue`, `twinwright worker` and
  `twinwright queue`. Workers claim runs under fenced leases, renew them while
  executing, and take over runs whose lease lapsed. Delivery is at-least-once
  with idempotent handlers and fencing. (ADR 0020, schema version 2)
- **Transactional fencing.** The ownership check runs inside the same
  transaction as the write it protects, and rejects both a superseded fence and
  an expired lease. The monotonic committed-fence record keeps this correct under
  clock skew.
- **Ownership audit log.** `run_ownership_log` records lease acquisition,
  release, takeover, fencing rejections and run completion, separately from the
  run ledger.
- **OTLP delivery.** `twinwright trace --format otlp --otlp-endpoint <url>` posts
  spans to an OpenTelemetry collector and reports its status code. A rejection is
  an error, not a silent success. (ADR 0021)
- **Prometheus metrics.** `twinwright worker --metrics-addr` serves in-process
  worker counters plus ledger-derived gauges. Unauthenticated and off by default.
- **`twinwright version` and `twinwright doctor`.**
- **CI.** `.github/workflows/ci.yml` runs gofmt, vet, build, tests, the race
  detector, a deterministic end-to-end script, `go mod tidy` verification,
  gitleaks and govulncheck. `integration.yml` runs the suite against PostgreSQL
  16 and 17 service containers, the distributed tests under `-race`, and the
  end-to-end script against PostgreSQL.
- **Developer experience.** `Makefile`, `docker-compose.yml` for PostgreSQL and
  an OpenTelemetry collector, `scripts/e2e.sh`, `CONTRIBUTING.md`, `SECURITY.md`,
  issue and pull request templates.

### Fixed

- **A crash between persisting `model.request` and recording `model.response`
  made a run permanently unreplayable.** On resume the runner appended a second
  request, leaving more requests than responses forever. The request payload is
  derived only from the task, provider, model, transcript and exposed operations,
  so a resumed attempt regenerates it byte for byte; the runner now reuses the
  abandoned request, and fails loudly if a trailing request differs.
- **World seeds and cent amounts could overflow on PostgreSQL**, whose `INTEGER`
  is 32-bit. Those columns are `BIGINT` there.
- **Event sequence allocation was not portable**: PostgreSQL rejects a bare
  column beside an aggregate. The query groups explicitly, which is identical in
  behaviour on SQLite.
- **Two upserts incremented an unqualified column**, which PostgreSQL rejects as
  ambiguous inside `DO UPDATE SET`.
- **Fork lineage flags were written as Go booleans into integer columns**, which
  only SQLite accepts.

### Changed

- **Line endings are pinned to LF** via `.gitattributes`, so `gofmt -l` and CI
  agree with a Windows checkout.
- Read-only commands (`inspect`, `trace`, `replay`, `evaluate`, `checkpoints`,
  `compare`, `counterfactual`) accept a PostgreSQL DSN and open it with
  `default_transaction_read_only=on`, so the server refuses writes.

### Removed

- **The standalone lease package and `twinwright lease`.** ADR 0018 put leases in
  a database separate from the runs they owned, which makes fencing
  unenforceable: with no transaction spanning both, a stale worker can pass the
  check and commit anyway. Run ownership now lives in the world store. Shipping
  two lease implementations with different guarantees would be a trap for the
  next reader, so the weaker one is gone rather than deprecated in place.
  Ownership is inspected with `twinwright queue --run <run-id>`.
  (ADR 0020 supersedes ADR 0018)

## Earlier work

Milestones 1 through 14 are recorded in `docs/adr/0001` through `0018`: the world
compiler, the multi-service company world, generalized world definitions,
checkpoints and execution forks, the chaos engine, principal authorization,
declarative assertions, the counterfactual debugger, ledger-derived traces,
multiple agent providers, Twinwright Bench, experimental shadow mode, and
optional container execution.
