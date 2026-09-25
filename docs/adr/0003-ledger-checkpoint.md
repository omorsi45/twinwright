# ADR 0003: Atomic local tool effects and checkpoints

Status: Accepted, 2026-09-24

Persist ordered run events and full provider input/output. Give each tool call a stable ID. For mutations, commit state change, mutation event, saved tool result, and resume cursor in one SQLite transaction. Retrying a committed call returns its saved result. Persist the one-shot fault similarly. Pause only between completed steps. This provides local side-effect safety while leaving full replay and external-service exactly-once semantics for later design.
