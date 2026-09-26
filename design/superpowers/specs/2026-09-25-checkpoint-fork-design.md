# Twinwright checkpoint and fork design

Date: 2026-09-25
Status: implementation design
Baseline: Milestone 3 branch `milestone-3-world-definition`, commit `fea47fd`, PR 2

## Intent

Milestone 4 lets a developer choose a committed point in a run, inspect it as a checkpoint, and continue from that point in a separate world. The parent run and its world remain untouched. The fork inherits the exact prior observations and world state, records its lineage, and can use a different provider, model, or one-time fault after the fork. A comparison command reports differences between parent and child outcomes.

## Approaches considered

1. Copy all SQLite tables at every event. This is simple to restore but duplicates data, couples checkpoint size to the number of services, and obscures whether recorded actions can reconstruct state.
2. Rebuild from the seed and the ledger prefix. This uses the existing deterministic dispatcher and verifies that each recorded tool result matches before creating a child. It costs replay time and needs a carefully bounded prefix interpreter. **Chosen.**
3. Apply `state.mutation` payloads directly. Current mutation events omit enough fields to reconstruct all service rows, so this would require a second state mutation protocol before forking works.

## Checkpoint contract

A checkpoint identifies a source run, a committed event sequence, the manifest digest, a format version, and a SHA-256 digest of the canonical event prefix. Supported boundaries are the end of a `model.response`, `tool.response`, `execution.paused`, or `execution.completed` transaction. A `tool.request`, `state.mutation`, or `model.request` cannot be selected because its enclosing action is incomplete. Since a tool dispatch may emit several events in one transaction, the boundary is the final `tool.response`. Selecting any other sequence returns the nearest supported boundaries rather than silently rounding.

`twinwright checkpoints <run-id>` lists these boundaries with sequence, event type, model turn, tool call count, and deterministic checkpoint identity. A checkpoint record is stored when a fork is made; listing alone does not mutate the source. The parent may later resume, but an existing checkpoint's prefix and hash remain fixed. Forking requires the source run to be paused or completed so its ledger cannot change during reconstruction. A completed source is verified using existing replay first; a paused source undergoes the same prefix verification used for the fork.

## Prefix reconstruction

Open the source database for reading and verify the manifest digest. Read the event prefix through the selected sequence and reject gaps, malformed payloads, unsupported error events, or incomplete transaction groups. Seed a temporary in-memory world with the source seed, scenario, and digest. Create a temporary run with the source run ID and original provider, model, task, and fault setting, preserving deterministic entity IDs. Reproduce recorded model requests and assistant responses through the store, and dispatch recorded tool calls with the original call IDs. Compare produced event types and JSON payloads, tool results, and the transcript at the selected boundary against the source prefix. Reject divergence before touching the destination database.

The reconstructed world rows and run transcript form a verified checkpoint. World state is copied once into a new isolated world in the destination database, in one transaction. The copy uses an explicit allowlist of the same world-scoped tables replay compares, not a SQLite file copy. A child run gets a new run ID, the verified transcript, step count, provider/model selection, fault configuration for future calls, and a lineage row. Tool results from the prefix are copied under the child run ID so prior call IDs remain idempotent. The child's new event ledger starts with `execution.forked` containing parent run ID, source event sequence, checkpoint ID, and manifest digest. Inherited history is available through its transcript and lineage; child events represent only its suffix. The parent is never updated.

## Continuation and comparison

`twinwright fork <run-id> --at-event <seq> --manifest <path> --db <path>` creates a paused child by default, reports its IDs and checkpoint, and optionally runs it with `--agent`, `--model`, `--fault`, and `--steps`. Default provider and model match the parent. A new fault applies only after the fork, and its operation ID must exist in the manifest. A child with pending tool calls resumes them before another provider turn. Changing providers/models is recorded in lineage; source observations are held fixed.

`twinwright compare <parent-run-id> <child-run-id>` reports parent and child status, evaluation checks when completed, tool trajectory from the fork point, mutation events, final state table differences, and available elapsed time/model usage. Missing telemetry is reported as unavailable rather than invented. Comparisons require the child lineage to name the supplied parent.

The existing completed-run replay command remains unchanged for old runs. Forked-run replay reconstructs its named parent checkpoint first, then verifies the child's suffix from that state. If this cannot be completed within the milestone, the CLI must explicitly reject forked-run replay; it must never report success from an ordinary seed replay.

## Atomicity, compatibility, and limits

Checkpoint reconstruction occurs in memory. Creating the child world, copying rows and prior results, inserting the child run and lineage, and emitting `execution.forked` commit together. Any failure leaves no child world or run. A fresh database migrates automatically; older databases gain lineage/checkpoint tables without changing existing run rows. The versioned manifest digest and checkpoint format gate restoration. Concurrent writes to a paused source are rejected by rechecking its ledger tip and prefix hash immediately before the creation transaction.

The first release supports the four built-in local services and existing seed profiles. A custom registered behavior can compile and run but cannot fork until it implements an explicit state copy/reconstruction contract; the CLI rejects that case. This boundary is reported in documentation.

## Verification

Tests cover every accepted and rejected event boundary, deterministic checkpoint identities, parent immutability, prefix tamper rejection, child isolation, inherited transcript and idempotency, pending tool calls, changed provider/model/fault, atomic failure rollback, lineage, old database migration, exact manifest and format compatibility, comparison reports, and replay of a forked completed run or its explicit rejection. CLI smoke tests fork a company incident before and after a mutation, run the child, and show a useful difference from the parent without a paid model call.
