# ADR 0015: Experimental shadow mode

Status: accepted, 2026-09-25

The roadmap asks for a shadow-mode architecture that can observe external events, simulate agent behavior, record proposed actions, and compare them to human actions, without executing production writes by default.

Connecting the existing dispatcher to live HTTP backends was rejected for this milestone. It couples simulation to credentials too early and makes accidental writes too easy. Instead, a separate `shadow` package accepts an observe-only config, reads a JSONL observation log from under `examples/`, runs the agent against a local SQLite world, and compares proposed tool calls to observed actions by operation ID and canonical arguments. The comparison reports matches and residuals without winner language.

`mode` must be `observe`, `label` must be `experimental`, and `allow_writes: true` is rejected. Secret environment variable names may be listed so operators know which credentials belong to a future integration; values are never logged. No production connectors ship here. A future write-capable shadow path requires its own ADR and explicit configuration.
