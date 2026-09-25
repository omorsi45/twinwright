# ADR 0008: Persisted deterministic chaos rules

Status: accepted, 2026-09-25

Twinwright stores a validated version 1 chaos policy per run. Rules select compiled operation IDs and advance ordered matching-call counters inside each tool transaction. Ledger events, service writes, snapshots, hidden outcomes, saved tool observations, and counters commit atomically. Reusing a call ID returns the saved observation without replaying the effect.

The ten effects cover pre-execution failures, virtual latency, misleading reads and responses, actor writes, and a committed write whose response is lost. The latter returns status 0 to the agent while storing the handler's real outcome in an audit table. A fresh call ID can duplicate the write. `ambiguous-commit` demonstrates this with safe reconciliation and an unsafe retry of a 500-cent refund.

Replay loads the stored policy and regenerates decisions from recorded assistant turns. It compares semantic events and persisted policy, counters, snapshots, and hidden outcomes. Checkpoint reconstruction reexecutes only the selected prefix. Fork creation copies that prefix state, while an explicit replacement policy starts with no counters. A lineage flag records replacement so fork replay can reconstruct the correct child start state, even if the replacement policy has the same digest as its parent.

The older `--fault` path and existing run ledgers keep their original behavior. Policy parsing happens before world creation. Latency is simulated and does not sleep. No external network faults, real permission model, distributed concurrency, or live model result is claimed by these fixtures.
