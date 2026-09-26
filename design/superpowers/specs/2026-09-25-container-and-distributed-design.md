# Milestone 13: optional containerized world execution design

Roadmap section: lines 1116–1131.

## Intent

When a world needs process isolation (intentionally failing processes, network partitions, service restarts, realistic HTTP), Twinwright may optionally run a component in Docker. Default local SQLite worlds stay in-process. Kubernetes is out of scope.

## Recommended approach

A `container` package with an `Executor` interface: `Start`, `Stop`, `Addr`. The default implementation is `Local` (no-op, worlds stay in-process). `Docker` shells to `docker run` only when a world definition sets `runtime: docker` and the daemon is available. World definitions without that field are unchanged. Secrets never appear on the docker CLI argv; use env files outside the repo.

## Limits

- Optional. Missing Docker is a clear error only for docker-runtime worlds.
- No Kubernetes.
- Not implemented until Milestones 10–12 land on main or this is stacked deliberately.

# Milestone 14: distributed runtime design

Roadmap section: lines 1133–1159.

## Intent

Only after single-node semantics are solid. Evaluate PostgreSQL plus workers. Define consistency, delivery, idempotency, leases, and recovery before claiming anything. Do not claim exactly-once.

## Recommended approach

Write an ADR that freezes single-node guarantees first, then a design that maps call-ID idempotency and atomic local mutation onto Postgres + a lease table. No code until the ADR is reviewed and single-node PRs are merged.

## Limits

- Design and ADR only until Omar authorizes implementation.
- Accurate terminology: at-least-once delivery with idempotent handlers, not exactly-once.
