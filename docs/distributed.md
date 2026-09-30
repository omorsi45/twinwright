# The distributed runtime

Twinwright runs one incident on one process by default. It can also run a fleet
of workers over one database, which is what makes crash recovery something the
project measures rather than describes. This page is the picture of that, and
every database identifier below is checked against the schema by
`TestDistributedDiagramsNameOnlyRealSchemaIdentifiers` in
`internal/store/diagrams_test.go`, so a renamed table breaks the diagram that
names it.

What this is: a multi-worker runtime over one database. What it is not: a
multi-region system, a broker, or a shard map. ADR 0020 records why a broker was
rejected, since the queue needs claim-one-row-atomically and
take-over-on-expiry, which PostgreSQL provides in the same transaction as the
lease.

## Topology

`twinwright run --enqueue` queues a run. `twinwright worker` claims, executes
and finishes runs. `twinwright queue` reports depth, fleet and ownership, and
writes nothing.

```mermaid
flowchart LR
    subgraph cli["twinwright processes"]
        E["run --enqueue"]
        W1["worker A"]
        W2["worker B"]
        Q["queue"]
    end
    subgraph db["one store, one transaction boundary"]
        WQ[("work_queue")]
        RL[("run_leases")]
        OL[("run_ownership_log")]
        WK[("workers")]
        EV[("runs and events")]
    end
    E -->|"enqueue, state runnable"| WQ
    W1 -->|"claim oldest claimable"| WQ
    W2 -->|"claim oldest claimable"| WQ
    W1 -->|"acquire, fence plus one"| RL
    W2 -->|"acquire, fence plus one"| RL
    W1 -->|"fence proved, then write"| EV
    W2 -->|"fence proved, then write"| EV
    RL -->|"every transition"| OL
    W1 --> WK
    W2 --> WK
    Q -->|"read only"| WQ
    Q --> RL
    Q --> WK
```

A worker claims the oldest run that is runnable, or one marked leased whose
lease has expired, which is exactly what a killed process leaves behind.
Recovery needs no janitor: the next poll picks the run up. On PostgreSQL the
candidate row is taken with `FOR UPDATE ... SKIP LOCKED`, so simultaneous
pollers take different runs instead of contending. SQLite has a single writer,
so the transaction alone provides that exclusion.

Ownership history is in `run_ownership_log`, not in the run's ledger. Fork
lineage and checkpoint reconstruction digest a run's event prefix, so if
ownership transitions lived there an identical agent trajectory would digest
differently depending on which worker executed it and how often the lease
changed hands. Keeping the ledger a pure record of what the agent did is what
lets a distributed run and a single-node replay produce byte-identical ledgers.

## A worker dies mid-run

Delivery is at-least-once. Exactly-once delivery is not claimed anywhere in
this codebase, because it is not achievable across a process boundary and a
database. The effect is made idempotent instead: the dispatcher looks up the
call by its identifier before executing, and a repeated call returns the
recorded status and body while committing nothing. The same identifier with
different arguments is refused rather than answered from the cache, so a cache
hit can never stand in for a different request.

```mermaid
sequenceDiagram
    participant A as Worker A
    participant B as Worker B
    participant Q as queue table
    participant L as lease table
    participant S as world store

    A->>Q: claim oldest claimable run
    Q-->>A: run granted
    A->>L: acquire, fence 1
    L-->>A: owner A, fence 1
    A->>S: tool write, fence proved in the same transaction
    S-->>A: committed, committed_fence 1
    Note over A: process killed here, lease row left behind
    B->>Q: claim oldest claimable run
    Q-->>B: the same run, its lease expired
    B->>L: acquire, fence 2
    L-->>B: owner B, fence 2
    B->>S: recorded calls answered from the ledger, then continue
    S-->>B: no duplicate effect
    A->>S: late write, still presenting fence 1
    S-->>A: refused, 1 is below committed_fence 2
    B->>S: finish the run
```

The last two exchanges are the ones worth reading twice. A stalled worker that
wakes up and writes is the failure a lease alone does not prevent, and the
refusal is why a duplicate delivery is safe rather than merely unlikely. The
project's bench suite injects exactly this: `examples/bench/flagship.yaml`
crashes the first worker at three points in a sixteen-call trajectory and
requires a takeover, a duplicate delivery and a refused stale commit on each.

## The lease, and why the clock is not trusted

```mermaid
stateDiagram-v2
    [*] --> Unleased
    Unleased --> Owned: acquire, fence plus one
    Owned --> Owned: heartbeat renews expires_at
    Owned --> Expired: expires_at passes with no renewal
    Expired --> Owned: another worker acquires, fence plus one
    Owned --> Unleased: run finished, lease released
    Owned --> Fenced: renewal rejected, execution cancelled
    Fenced --> [*]
    note right of Expired
        Expiry alone disqualifies the old owner, because
        the instant a lease lapses another worker may take
        the run. The presented fence must also be at least
        committed_fence, the highest that has committed.
        That condition involves no clock at all, so it is
        still correct under skew between a worker and the
        database, which is the case a TTL check alone gets
        wrong.
    end note
```

Every acquisition increments the fence, including re-acquisition by the same
owner. A worker that lost contact and reconnected must not keep using its old
token, because the run may have been taken over and handed back in between.

While a run executes the worker renews on a timer, and a rejected renewal
cancels the execution context immediately. Continuing would spend model calls on
work this worker is no longer permitted to commit.

## Seeing it happen

```
twinwright bench --suite-file examples/bench/flagship.yaml
twinwright queue --dsn <dsn>
twinwright demo --dir <dir>     # step 8 crashes a worker and shows the takeover
```

The crash tests advance a controlled clock past the lease TTL rather than
sleeping it out, so the whole distributed section of the demo finishes in about
half a second. Every recovered run is replayed and required to verify, which is
what makes "byte-identical ledgers" a measurement instead of a claim.
