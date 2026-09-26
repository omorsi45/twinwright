# ADR 0018: Lease ownership with fencing tokens

Status: accepted, 2026-09-25

ADR 0017 froze single-node guarantees and rejected claiming a multi-worker runtime before leases, delivery semantics, and recovery were defined. This ADR introduces the first piece of that foundation: time-bounded lease ownership with fencing tokens, without rewriting the world store onto Postgres or claiming a full distributed cluster.

Workers acquire a named lease (`run/R-1`, for example) for a TTL of at least one second. Each successful acquire or reclaim increments a fencing token. Renew and release succeed only when the caller presents the current owner and token. An expired lease can be reclaimed by another owner with a new token. Release soft-clears the owner so the token sequence keeps climbing; a stale holder cannot renew after release.

Delivery remains at-least-once. A crashed worker may retry the same unit of work. Handlers stay idempotent by tool call ID. Exactly-once delivery is not claimed. The lease store is a separate SQLite file from the world database. Postgres-backed world storage and multi-node orchestration stay out of scope until a later ADR maps every ADR 0017 guarantee onto that design.

The CLI surfaces `twinwright lease acquire|renew|release|status`. JSON responses set `experimental: true` and `delivery: at_least_once`.
