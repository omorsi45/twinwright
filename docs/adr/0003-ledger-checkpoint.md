# ADR 0003: Atomic local tool effects and checkpoints

Status: Accepted, 2026-09-24

Persist ordered run events, normalized provider requests, and provider response items. Record model request intent before invoking the provider. Preserve raw response items when decoding fails so an invalid tool call remains inspectable. Give each tool call a stable ID. For mutations, commit state change, mutation event, saved tool result, and resume cursor in one SQLite transaction. Retrying a committed call returns its saved result only when operation and arguments match. Persist the one-shot fault similarly. Pause only between completed steps. Resume requires the original manifest digest and provider model. This provides local side-effect safety while leaving full replay and external-service exactly-once semantics for later design.
