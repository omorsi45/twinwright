# ADR 0004: Isolated verification replay

Status: Accepted, 2026-09-25

Replay completed fictional billing runs by feeding recorded assistant responses to the existing runner in a fresh in-memory SQLite world, opening the source database without creation or migration. Preserve the source run ID to preserve refund IDs. Preserve JSON number representations from the ledger. Compare semantic events, saved tool results, transcript, and complete billing state. Reject incomplete or failed model-turn histories. This verifies current-runtime reproducibility without network calls or source writes. Counterfactual forks, recovery replay, historical runtime compatibility, and external side-effect replay require later designs.
