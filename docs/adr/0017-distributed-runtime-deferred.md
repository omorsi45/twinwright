# ADR 0017: Distributed runtime is not claimed yet

Status: accepted, 2026-09-25; superseded in part by ADR 0018, completed by ADR 0019 and ADR 0020

The distributed runtime this ADR declined to claim now exists. ADR 0019 added
PostgreSQL storage and ADR 0020 added multi-worker execution, which maps each
guarantee frozen below onto the new storage and lease model. The freeze itself
stands as the contract that work had to preserve.

Single-node Twinwright on SQLite now covers compile, run, resume, chaos, authorization, assertions, counterfactuals, traces, providers, bench, shadow observe-only, and optional containers. The roadmap's distributed runtime (PostgreSQL, workers, leases) must not be claimed until consistency, delivery, idempotency, lease ownership, and recovery are defined and implemented.

This ADR freezes the current single-node guarantees that any later distributed design must preserve:

- Every world query is scoped by `world_id`.
- Tool call IDs are idempotent within a run.
- Local mutating tool effects commit atomically with their ledger events.
- Evaluation and replay do not mutate the source database.
- Runs without chaos or auth policies keep identical ledgers across versions that claim compatibility.
- Delivery in a future multi-worker system will be described as at-least-once with idempotent handlers, not exactly-once.

No full distributed implementation ships with this ADR. ADR 0018 adds SQLite lease ownership with fencing tokens as the first foundation piece. A later change that introduces Postgres or multi-node workers needs a new ADR that maps each guarantee above onto the new storage and lease model before claiming production readiness.
