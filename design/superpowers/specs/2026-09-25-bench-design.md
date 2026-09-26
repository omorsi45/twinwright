# Milestone 11: Twinwright Bench design

Roadmap section: `briefs/2026-09-25-platform-roadmap.md`, "MILESTONE 11: BENCHMARK SUITE" and "BENCHMARK RUNNER" (lines 999–1095).

## Intent

Ship a serious, public, machine-readable benchmark over the existing company and billing worlds. Cases use deterministic ground truth (scenario evaluation or assertion files). Do not invent thousands of trivial template clones. The first suite is a curated set spanning the roadmap categories, grown from real chaos, auth, and scenario fixtures already in the tree.

## Approaches

1. One hard-coded Go table of cases. Rejected for a public suite: contributors cannot add a case without editing Go.
2. A versioned suite YAML plus a runner package (chosen). Cases reference manifests, scenarios, optional chaos/auth/assertion files, and the success judge. The CLI loads a named suite from `examples/bench/`.
3. Replaying recorded ledgers as the only bench path. Rejected: the roadmap wants agent runs under controlled conditions; scripted fixtures are the CI default, live agents are optional.

## Design

### Suite file

Version 1 YAML under `examples/bench/standard.yaml`:

- `version: 1`
- `suite: standard`
- `cases[]` with: `id`, `category` (`reliability` | `reasoning` | `safety` | `security` | `recovery` | `long_horizon`), `world` (`billing` | `company`), `scenario`, optional `chaos`, `auth`, `assertions`, `recovery` (`safe`/`unsafe` for ambiguous-commit), `fault`, `steps`, and expected dimension tags used when aggregating (`task`, `safety`, `authorization`, `recovery`, `duplicate_effects`).

Paths in the suite are relative to the repository `examples/` root unless absolute. The parser rejects unknown fields, unknown categories, missing required fields, and paths outside `examples/`.

### Runner

`internal/bench` loads the suite, compiles the needed manifests once, runs each case in an isolated temp database (or a caller-supplied work directory), selects the provider the same way the CLI does, judges with assertion files when present else `eval.Evaluate`, and records per-case:

- `id`, `category`, `status` (`passed` | `failed` | `error`)
- `run_id`, `checks` (failed names), `tool_calls`, `model_turns`, `wall_ms`, `duplicate_refunds` when measurable
- dimension booleans derived from tags and outcome (a failed safety-tagged case lowers Safety Compliance)

Aggregates in the report (no "winner" language):

- scenario counts
- Task Success, Safety Compliance, Authorization Safety, Recovery Success (percent of tagged cases that passed)
- Duplicate Effects (percent of tagged cases that produced more than one refund where the ground truth forbids it, or from analysis `unsafe_retry`)
- Median Tool Calls, Median Latency (wall clock of the case run)

Output is one JSON document. The CLI also prints a short text summary matching the roadmap shape.

### CLI

```text
twinwright bench --suite standard --agent scripted [--model ...] [--base-url ...] [--db-dir path] [--out report.json]
```

Default agent is `scripted` so CI stays deterministic. Live agents are allowed and labeled; no live call is claimed verified.

`twinwright compare <a.json> <b.json>` detects bench reports (presence of `suite` and `cases`) and emits measurement deltas without declaring a winner. Existing run-id compare stays when both args look like run IDs.

### Limits

- The first suite is on the order of dozens of cases, not hundreds. The README states the count honestly.
- Scripted fixtures are the verified path. Live provider bench runs need keys and are nondeterministic.
- Pause/resume recovery cases use `--steps` then `resume` inside the runner when a case marks `resume: true`.
- Compare of bench reports is measurement-only.
