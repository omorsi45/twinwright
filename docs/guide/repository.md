# Repository layout and architecture decisions

This page was moved out of the project README when the README was rewritten as the Twinwright Cloud overview. It holds the README sections "Repository layout", "Architecture decisions", unchanged except where noted. The ADR count was corrected from 29 to 34, and the layout lists the packages, scripts and documents added since it was written. Paths in backticks are relative to the repository root. [Back to the guide index](README.md).

## Repository layout

```text
cmd/twinwright/       CLI

internal/
  agent/              provider boundary and durable runner
  dispatch/           validated tool execution
  compiler/           API and world compilation
  behavior/           behavior registry
  billing/ crm/ ticketing/ messaging/   the four service simulations
  store/              SQLite and PostgreSQL state, ledger, queue, leases
  pgsql/ pgtest/      PostgreSQL driver wrapper, placeholder translation, test helpers
  worker/             multi-worker claim, lease renewal and takeover loop
  chaos/              deterministic fault policies and state
  authz/              principal policies and authorization decisions
  eval/               deterministic scenario evaluation and run analysis
  assertion/          declarative run assertions
  replay/             verification replay
  checkpoint/         checkpoint discovery and reconstruction
  fork/               fork execution and trajectory comparison
  counterfactual/     intervention analysis over forks
  bench/              suite parser, case runner, and report aggregates
  shadow/             experimental observe-only shadow simulation
  container/          optional local or Docker sidecar executor
  trace/ metrics/     ledger traces, OTLP export, Prometheus exposition
  redact/             secret redaction before storage or display
  crashpoint/         named crash boundaries and the hook that fires them
  crashmatrix/        the crash matrix harness behind the recovery tests and distributed bench cases
  gate/               comparison of a bench report with an approved baseline
  perf/               the platform workloads behind `twinwright perf`
  buildinfo/          build version and provenance

examples/
  billing/ company/   the minimal reference world and the four-service world
  chaos/ security/    failure policies and principal policies
  assertions/ counterfactual/ bench/   assertion files, intervention files, suites
  shadow/ container/  observe-only configs with sample observations, sidecar configs
  agents/             a Python reference agent for the exec protocol

scripts/              e2e.sh, demo-stack.sh, live-model.sh, perf.sh
deploy/               collector and Prometheus configuration for the Compose stack

docs/adr/             architecture decision records
docs/distributed.md   fleet topology, takeover sequence, lease lifecycle
docs/performance.md   measured runtime figures and the machine that produced them
docs/security-review.md   the review pass, with the test behind each claim
docs/architecture.md  components, the path of one tool call, trust boundaries, the ADR index
docs/claims.md        every claim the README makes, with the test or CI job that proves it
docs/guide/           the detail the README links to
docs/protocol/        the agent protocol and its JSON Schemas
docs/benchmarks/      committed perf reports
docs/live-model-run.md   one recorded run of a local model
```

## Architecture decisions

Every runtime contract is documented as an ADR under `docs/adr/`, 34 of them, each
recording what was decided, what was rejected, and the limits that came with it.
The ones that constrain everything else: `0001` Go and SQLite, `0002` OpenAPI plus
explicit behavior bindings, `0003` atomic tool effects and checkpoints, `0004`
isolated verification replay, `0009` runtime principal authorization, `0017` the
single-node guarantees frozen as the contract a distributed runtime had to preserve,
`0020` multi-worker execution with fenced ownership, `0025` honest bench
aggregation, `0028` the security review, and `0029` crash boundaries, the delivery
bound and what is idempotent, which corrects what `0020` overstated. Since then: `0030` the
regression gate, `0031` provider recording and the real-model run, `0032` the verified
observability stack, `0033` the agent protocol and container isolation, and `0034` benchmark
methodology. [docs/architecture.md](../architecture.md) lists all 34.
