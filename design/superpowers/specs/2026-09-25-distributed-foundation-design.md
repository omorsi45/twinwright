# Milestone 14: distributed runtime foundation design

Roadmap: lines 1133–1159. ADR 0017 deferred claims; this milestone implements the recommended first foundation after Omar authorized continuing.

## Intent

Define delivery and lease semantics in code, without claiming a multi-node production cluster.

## Recommended slice (chosen)

1. ADR 0018 states consistency, at-least-once delivery, idempotency via call IDs, lease ownership, fencing tokens, and recovery. Supersedes the "no implementation" half of ADR 0017 while keeping its single-node guarantee list.
2. `internal/lease`: SQLite-backed leases (same modernc driver as the store) with acquire, renew, release, and reclaim-expired. Fencing tokens increase on each acquire so a late holder cannot commit after loss.
3. CLI `twinwright lease` for acquire/renew/release/status against a DB path. Experimental label.
4. No Postgres migration of the full world store in this PR. A Postgres-backed world store remains a follow-up once leases and fencing are proven. No Kubernetes. No exactly-once claims.

## Rejected

- Rewriting the entire store onto Postgres in one PR: too large, unverifiable without a live server, and would mix schema migration risk with lease design.
- Claiming "distributed runtime ready" when only leases exist.
