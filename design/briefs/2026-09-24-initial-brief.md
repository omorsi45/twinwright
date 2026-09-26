You are the principal engineer helping me build an ambitious open-source project called **WorldForge**.

WorldForge is an infrastructure platform for testing autonomous AI agents inside realistic, executable digital twins of enterprise software environments.

The long-term vision is:

**Company software environment → automatically compiled executable agent world**

A user should eventually be able to provide OpenAPI specifications, database schemas, tool definitions, permissions, sample events, and related metadata. WorldForge will turn those inputs into a stateful simulated environment where AI agents can execute realistic business workflows without touching production systems.

The final system may eventually include:

- automatic environment generation from OpenAPI/MCP/database schemas
- stateful simulated services
- realistic entities and synthetic data
- cross-service relationships
- agent identities and scoped permissions
- RBAC/ABAC
- agent tool execution
- transaction and side-effect tracking
- idempotency
- durable execution
- checkpointing
- execution replay
- failure recovery
- chaos/fault injection
- trace collection
- observability
- agent evaluations
- regression scenarios
- counterfactual replay
- model comparisons
- shadow execution
- human approval gates
- Docker/container isolation
- distributed execution
- multiple model/agent providers
- benchmark datasets

However:

**DO NOT attempt to implement all of this now.**

The project must be built like serious infrastructure software, not as an AI-generated collection of loosely connected features.

## Engineering principles

1. Architecture must remain modular.
2. Core infrastructure must not depend tightly on one model provider.
3. Agent frameworks must interact with WorldForge through stable interfaces.
4. Environment state must be deterministic when possible.
5. Every externally visible side effect must be auditable.
6. Execution must eventually support replay.
7. Simulated tools must behave like stateful services, not simple mocked responses.
8. Core logic must have strong automated testing.
9. Avoid unnecessary abstractions.
10. Do not create functionality merely to make the repository appear large.
11. Prefer deep implementation of a few capabilities over shallow implementation of many.
12. Explain significant architectural decisions before implementing them.

## Initial technology direction

Tentatively consider:

- Go or Rust for the runtime/execution infrastructure
- Python for agent/evaluation integrations
- PostgreSQL for durable state
- Docker for environment isolation
- OpenTelemetry-compatible tracing
- TypeScript/React later for a UI

These choices are not final.

You must evaluate them and challenge them if better alternatives exist.

## Phase 0: Architecture

Before writing significant production code:

1. Define the major system boundaries.
2. Define the core domain model.
3. Define how a World is represented.
4. Define how tools/services are represented.
5. Define how state mutations occur.
6. Define how an agent invokes a tool.
7. Define how execution events are recorded.
8. Define how checkpoint/replay could eventually work.
9. Define what must be deterministic.
10. Identify the hardest technical risks.
11. Propose repository structure.
12. Write Architecture Decision Records for major choices.

Do not overdesign components that are not needed for the first milestone.

## Milestone 1

Build only this vertical slice:

### Input

A user provides a small OpenAPI specification describing a fictional support/billing service.

Example resources might include:

- customers
- subscriptions
- invoices
- charges
- refunds

### World compilation

WorldForge parses the OpenAPI specification and constructs a local executable representation of those tools.

The generated service must be **stateful**.

For example:

If:

`POST /refunds`

is called successfully, subsequent:

`GET /charges/{id}`

must reflect that refund.

Do not simply return hardcoded fake responses.

### Seeded world

Generate deterministic synthetic data from a seed.

For example:

- customers
- invoices
- charges
- subscription states

Running the same world with the same seed should reproduce the same initial environment.

### Agent execution

Expose the generated tools to one agent.

Initially support only one provider or agent adapter.

Do not build multi-agent orchestration yet.

The agent should receive a task such as:

> Customer C-104 says they were charged twice. Investigate the account and refund only the duplicate charge if appropriate.

The agent must inspect the simulated tools and determine the proper actions.

### Event ledger

Every important event must be persisted:

- execution started
- model invocation
- tool request
- tool response
- state mutation
- error
- retry
- execution completed

Events must have stable IDs and timestamps.

Design this event model carefully because future replay functionality will depend upon it.

### Chaos injection

Implement one controlled failure mechanism.

For example:

- specified tool call returns HTTP 503 once
- configurable artificial latency
- malformed response
- timeout

The agent execution should experience the failure naturally.

### Checkpoint/replay

Implement a minimal form of checkpointing.

I should be able to stop an execution after several steps and resume without recreating already completed state mutations incorrectly.

Prevent duplicate side effects when possible.

### Evaluation

At the end of the scenario, evaluate the world state programmatically.

For the duplicate-charge scenario:

- correct charge identified
- no legitimate charge refunded
- refund amount correct
- only one refund created

Do not ask another LLM whether the agent succeeded when deterministic verification is possible.

## CLI goal

I eventually want something conceptually similar to:

`worldforge build examples/billing/openapi.yaml`

then:

`worldforge run duplicate-charge --agent <provider>`

and:

`worldforge inspect <run-id>`

The exact API can evolve.

## Important constraint

Do not immediately start producing thousands of lines of code.

First return:

1. proposed architecture
2. data model
3. event model
4. repository structure
5. technology decisions
6. major risks
7. exact Milestone 1 implementation plan
8. what you intentionally will NOT build yet

Then wait for me to approve or modify the architecture before implementing Milestone 1.

Throughout development, behave as a senior infrastructure engineer rather than a code generator.

When encountering an architectural ambiguity, explain the tradeoffs instead of silently choosing an arbitrary solution.