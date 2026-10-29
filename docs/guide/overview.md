# Overview, scope and philosophy

This page was moved out of the project README when the README was rewritten as the platform overview. It holds the README sections "Why Twinwright", "What Twinwright does", "Design philosophy", "Building on Twinwright", unchanged except where noted. Paths in backticks are relative to the repository root. [Back to the guide index](README.md).

## Why Twinwright

Autonomous agents do more than generate text. They call tools, mutate business state, retry failed operations, cross service boundaries, and continue after partial failures.

Traditional mocks are good at returning canned responses. They are much less useful for answering questions like:

- Did the agent refund the right charge?
- What happens if the refund commits but the response is lost?
- Can execution resume after a crash without duplicating side effects?
- Can the exact run be reconstructed, verified, and forked from an earlier point?
- Does a malicious ticket comment get the agent to leak another customer's data?

Twinwright provides **isolated, stateful software environments** where those behaviors can be tested without touching production systems.

## What Twinwright does

Twinwright turns API contracts plus explicit behavior definitions into executable local worlds that agents can interact with through normal tool calls.

A run can:

- create a deterministic world from a seed, expose stateful tools to an agent, and
  record model and tool activity in an ordered event ledger
- commit local side effects atomically, and make repeated tool calls idempotent by
  stable call ID
- inject deterministic chaos policies, controlled failures, and ambiguous outcomes
  such as timeout-after-commit
- run an agent as a principal whose permissions are enforced at runtime, and
  measure blocked prompt-injection attempts
- pause and resume durable execution, and survive a provider that hangs or cuts a
  completion short: the per-attempt deadline is configurable, a truncated
  completion is never consumed as a decision, and every interrupted attempt is
  recorded as a replayable event
- evaluate final state against deterministic ground truth, and check a run against
  declarative state, event, authorization and ordering assertions
- replay a completed run in an isolated world, restore valid checkpoints, fork a
  run with changed model or fault conditions, and compare parent and child
  trajectories
- fork a failed run at candidate events, change one variable per fork, and rank
  which events the failure was sensitive to
- summarize a run and render its ledger as a trace, including an OpenTelemetry
  JSON export
- drive a run with OpenAI Responses, an OpenAI-compatible chat endpoint, Anthropic
  Messages, or a scripted fixture
- run Twinwright Bench, a curated suite with deterministic ground truth and JSON
  reports
- experimentally observe recorded actions, simulate proposed local tool calls, and
  compare them without production writes, reporting which arguments diverged on a
  shared resource, where the two streams disagree about order, and which proposed
  actions a principal policy would refuse
- optionally start experimental local or Docker sidecars when a scenario needs
  process isolation
- store runs, ledgers and runtime state in SQLite for local work or PostgreSQL for
  a fleet, under versioned migrations
- execute queued runs on several worker processes that own a run under a fenced
  lease, take over a crashed worker's run, and cannot commit once fenced out
  (at-least-once delivery with idempotent handlers, never exactly-once)
- post ledger-derived spans to an OpenTelemetry collector, and serve Prometheus
  metrics from a worker

The project includes a minimal billing world and a multi-service company world
spanning billing, CRM, ticketing, and messaging. `docs/security-review.md` is the
review pass over each internal boundary, with the test that holds every claim.

## Design philosophy

The project intentionally prioritizes:

- state and consequences over canned mocks
- deterministic verification over LLM judging
- explicit behavior over inferred magic
- transactional safety over optimistic retries
- reproducibility over opaque agent traces
- small testable interfaces over framework-heavy abstractions
- honest limitations over unsupported claims

The billing world remains a deliberately small reference implementation even as richer worlds are added.

## Building on Twinwright

Twinwright is intended as infrastructure for work such as:

- autonomous-agent reliability testing
- tool-use evaluation
- long-horizon workflow testing
- failure and recovery experiments
- ambiguous side-effect testing
- regression testing across model versions
- cross-service agent evaluation
- execution replay and debugging
- counterfactual trajectory analysis
- safe experimentation before production deployment

Capabilities are versioned with the runtime and world manifest. The CLI and ADRs are the source of truth for exact supported behavior in a given revision.
