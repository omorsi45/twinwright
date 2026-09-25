# ADR 0007: Verified checkpoints and isolated forks

Status: Accepted, 2026-09-25

Twinwright lists checkpoints only at committed execution boundaries: a model response, a tool response, a pause, or completion. A checkpoint identifies the source run and event sequence, manifest digest, format version, and a digest of the ordered event prefix. Selecting an event inside a tool transaction fails.

Fork creation replays the selected prefix into an in-memory store and compares its events, saved tool results, and transcript with the source. It then creates a separate world and paused child run in one SQLite transaction. The transaction rechecks the parent ledger and copies the reconstructed world tables and prior tool results. The parent run and world remain unchanged. A lineage row records the parent run, checkpoint, original provider and model. The child ledger starts with `execution.forked`; its transcript and saved tool results carry the verified history.

The `fork` command can leave the child paused or continue it immediately with a chosen provider, model, or one-time fault. `inspect` exposes lineage. `compare` shows the parent suffix and child trajectory, completed-run evaluations, and differences across world tables. Latency and model usage are unavailable until the runtime records them.

Forking requires a paused or completed root source, a matching manifest, and a reproducible prefix. A changed prefix, tampered result or transcript, unsupported boundary, or failed transaction leaves no child. Only the currently implemented seeded scenarios and registered behavior handlers can be reconstructed. Nested forks and historical runtime versions are not yet supported. Replay of a completed fork verifies its parent prefix before checking the child suffix.
