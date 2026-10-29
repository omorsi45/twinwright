# Experimental shadow mode

This page was moved out of the project README when the README was rewritten as the Twinwright Cloud overview. It holds the README section "Experimental shadow mode", unchanged except where noted. Paths in backticks are relative to the repository root. [Back to the guide index](README.md).

## Experimental shadow mode

**Experimental.** Shadow mode does not connect to production systems. It reads an observe-only config, decodes a recorded observation source through a connector, simulates what an agent would do in a local Twinwright world, and compares proposed tool calls to observed human and system actions. Write mode and production adapters are rejected.

```bash
go run ./cmd/twinwright build examples/billing/openapi.yaml --out twinwright.manifest.json
go run ./cmd/twinwright shadow \
  --config examples/shadow/observe-only.yaml \
  --examples examples \
  --manifest twinwright.manifest.json \
  --scenario duplicate-charge \
  --agent scripted
```

### Observation connectors

A connector adapts a recorded external format into Twinwright observations, so
shadow evaluation does not require hand-transforming an export first. The
config's `source.type` selects one:

| `source.type` | Reads |
| --- | --- |
| `file`, `jsonl` | Twinwright's native observation JSONL: `kind`, `at`, `operation_id`, `arguments`, `actor` |
| `audit_log` | a sanitized audit export: `timestamp`, `actor`, `action`, `parameters`, `outcome` |
| `recorded_http` | captured HTTP interactions: `at`, `method`, `path`, `operation_id`, `query`, `body`, `status`, `actor` |

Decoding is a pure function of bytes. A connector opens no socket, resolves no host
and holds no credential, so a crafted observation file cannot turn shadow mode into
a request forwarder. The transport is a file confined under the examples root,
enforced on both the config path and the loader, and capped at 8 MiB. Unknown
fields are rejected rather than ignored, because a misspelled key would drop real
observed behaviour and leave a report looking clean. An attempt that did not take
effect is not an observed action: a denied audit entry and a 4xx or 5xx response
are dropped, so the comparison cannot accuse the agent of missing a step that never
happened. `recorded_http` requires an explicit `operation_id` rather than inferring
one from method and path, because a report that names the wrong operation is worse
than one that refuses to load.

**No connector has been tested against a live external system.** Every test runs
against fixture bytes, and the JSON output carries `live_external: false` so a
report cannot be mistaken for evidence of one. A webhook or event-stream transport
is deliberately absent: a listening socket taking unauthenticated input is a
different security posture from reading a file and needs its own ADR.

The JSON output always sets `experimental: true` and names the `connector` that
produced the observations. Secret env names listed in the config are never printed.
See `docs/adr/0015-shadow-mode.md` and `docs/adr/0022-shadow-connectors.md`.

### What the comparison reports

Matched, only-proposed and only-observed answer "did the same thing happen". On
their own they cannot express a run that did broadly the same work differently: a
proposed refund of 500 against an observed refund of 5905 on one charge came back as
two unrelated entries, one in each only-list, leaving the reader to notice they were
the same charge. Three classifiers report that instead.

- **`argument_divergences`** pairs the leftover actions that are the same operation
  on the same resource and names the fields that differ, with both values and a
  presence flag per side. Resource identity comes from the arguments that name the
  resource (`id`, or a key ending in `_id`, carrying a non-empty string), not from
  the operation name and not from a call's position: a refund of `CH-2` and one of
  `CH-9` are different charges, and adding a call site renumbers every position
  after it. A call that identifies no resource stays in the only-lists rather than
  being paired on a guess.
- **`timing`** reports the pairs the two streams sequence differently. Order is the
  timing property both sides carry; elapsed time is not, because a local
  simulation's wall clock has nothing to do with how long a human took. The report
  says so in `timing.elapsed_gap` instead of printing a zero, and carries
  `timing.observed_timestamps` as the denominator.
- **`policy`** screens each proposed action against a principal policy supplied with
  `--policy` and lists what it would refuse, with the permission, the stable deny
  reason and the run-local call number. A refusal stands even when the observed
  stream contains the same action, flagged `also_observed: true`: a human with other
  permissions doing something is not evidence that this principal may. The screen
  calls the same `internal/authz` entry point the dispatcher's
  enforcement delegates to, so a shadow verdict cannot drift away from an enforced
  one, and it reads the world without executing or writing anything.

Add `--policy examples/security/billing-support-policy.yaml` to the command above to
turn the screen on. A policy is parsed against the world's own manifest, so one
naming a permission the world has no operation for is refused here rather than
screening nothing. `examples/security/support-policy.yaml` is the four-service
version; `billing-support-policy.yaml` is the same principal scoped to the billing
examples. Neither grants `refunds.create`, which is what makes the fixture's proposed
refund a finding.

Without `--policy` the report carries `policy.evaluated: false` and the reason,
because an empty refusal list reads exactly like a clean screen. The comparison
remains deterministic and sorted, and still declares no winner: the observed stream
is what happened, not what should have happened. See
`docs/adr/0026-shadow-comparison-depth.md`.
