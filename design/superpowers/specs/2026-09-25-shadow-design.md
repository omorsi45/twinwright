# Milestone 12: shadow mode design

Roadmap section: `briefs/2026-09-25-platform-roadmap.md`, "MILESTONE 12: SHADOW MODE ARCHITECTURE" (lines 1097–1114).

## Intent

Add an experimental, clearly labeled shadow-mode path. Twinwright may observe external-shaped events through explicitly configured read-only adapters, simulate what an agent would do inside a local world, record proposed actions, and compare those proposals to observed human actions. It must not perform production writes. No real production systems are connected by default.

## Approaches

1. Wire the existing dispatcher to optional HTTP backends. Rejected: too easy to accidentally write, and couples simulation to live credentials early.
2. A separate `shadow` package with observe-only interfaces and a local simulator (chosen). Observations enter as recorded events. Proposed tool calls are produced by running the agent against a seeded local world. Comparison is a pure function over proposed vs observed action lists. Any future write adapter requires an explicit `allow_writes: true` config that this milestone does not enable.

## Design

### Config

Version 1 YAML (`examples/shadow/observe-only.yaml`):

- `version: 1`
- `mode: observe` (only allowed value in this milestone)
- `label: experimental`
- optional `source`: `file` with a path under `examples/` to a JSONL observation log
- optional `secret_env`: list of env var names whose values must never appear in logs (validated present or absent without printing values)

`mode: write` and any write endpoint fields are rejected.

### Observation log

JSONL records with `kind` of `human_action` or `external_event`, plus `at`, `operation_id`, `arguments`, and optional `actor`. The file source is read-only.

### Simulator

`shadow.Simulate` seeds a local world from a manifest and scenario, runs the configured provider for up to N steps without calling any external HTTP backend, and returns `ProposedAction` values (operation_id, arguments, call_id) taken from the transcript tool calls. Proposed actions are also written as ledger-compatible JSON for inspectability inside the local DB only.

### Compare

`shadow.Compare(proposed, observed)` reports matches, only-proposed, and only-observed by operation_id and canonical argument JSON. No winner language.

### CLI

```text
twinwright shadow --config examples/shadow/observe-only.yaml --manifest ... --scenario ... --agent scripted
```

Output is JSON with `experimental: true`, config digest, proposed actions, observed actions, and the comparison. The command refuses to start if the config enables writes.

### Limits

- Experimental and labeled in README and ADR.
- No production connectors ship in this milestone.
- Secret env names are listed; values are never logged.
- Write mode is a deliberate future milestone with its own ADR.
