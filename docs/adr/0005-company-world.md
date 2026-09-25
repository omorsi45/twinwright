# ADR 0005: Explicit multi-service company world

Status: Accepted, 2026-09-25

Milestone 2 adds stateful CRM, ticketing, and messaging beside the existing billing service in one local Go runtime. Each service stores world-scoped rows in SQLite, and the dispatcher invokes its handler inside the same transaction that saves the mutation event, tool response, idempotency record, and run progress. This preserves the original local atomicity and replay contract across service boundaries.

The compiler continues to require exact known operation routes, primitive argument schemas, and explicit behavior bindings. The company example is a second supported contract, not an arbitrary enterprise OpenAPI importer. That generalization belongs to Milestone 3. New service packages own their business behavior; the dispatcher only validates, selects the bound handler, and commits the shared transaction.

Scenario-specific seeds create incident, routine, and no-duplicate company variants. Deterministic evaluation reads final database state rather than the agent's final text. Verification replay reseeds the same variant and compares all relevant service tables, while historical billing-only runs keep their original billing comparison scope. The source run remains read-only.

Known limits: all services are simulated in one process and one SQLite database; there are no production connections, principals, or generalized schemas. Authorization and distributed execution require separate contracts in later milestones.