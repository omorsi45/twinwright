# Quick start and the twelve-step demo

This page was moved out of the project README when the README was rewritten as the Twinwright Cloud overview. It holds the README section "Quick start", unchanged except where noted. The sample output now shows the real twelve-step markers from an actual run. Paths in backticks are relative to the repository root. [Back to the guide index](README.md).

## Quick start

### Requirements

- Go 1.27 or newer
- No API key for deterministic scripted runs
- An OpenAI API key only for live model runs

Clone the repository and run the full test suite:

```bash
go test ./...
```

### See the whole system in one command

```bash
go run ./cmd/twinwright demo --dir ./demo-out
```

Twelve steps over one incident: compile the four-service world, run it under injected faults, inspect, trace, evaluate, replay, fork and compare, crash a worker and let another take over, fail the same incident on purpose and explain the failure, block a prompt injection and an over-privileged request, then block a regressed agent at the CI gate. Everything is scripted, so it needs no API key, no network and no container runtime.

Every step reads its evidence back out of what it just ran, and the first step that cannot produce its evidence stops the walkthrough with a non-zero exit. The excerpt below is the real output of steps 5, 8, 10 and 12 of an actual run; the other eight steps print in the same shape.

```text
=== 5/12 evaluate the declarative assertions
Twelve assertions written against the world's final state and the ledger's order, not against the agent's text.
evidence: 12/12 assertions held against the world's final state and the ledger's order

=== 8/12 crash a worker and watch another take over
Kill the worker mid-incident, let its lease expire, and require the zombie's late commit to be refused by fencing.
evidence: 3 crashes, 3 takeovers at fence 1 -> 2, 3 stale commit refused by fencing, replay verified on all 3

=== 10/12 explain the failure with a counterfactual
Re-run from each committed boundary with one thing changed, and report which change would have altered the outcome.
evidence: 32 interventions against correct_charge_identified, refund_amount_correct, exactly_one_refund. 16/16 agent-side changes corrected it, the latest at event 73; only 8/16 world-side changes did, none after event 32, so the world's opportunity closes and the agent's does not

=== 12/12 block a regressed agent at the CI gate
The same suite with a fixture that investigates, then claims the issue is resolved without making the write. The gate compares it with the approved baseline and must refuse it.
evidence: exit 1: 19 regressed cases in 21 compared, including billing-duplicate-charge
```

Event 32 is the refund dispatch and event 73 is the final model decision, after the support notification has already gone out: changing the world stops helping once the write is lost, while changing the agent still corrects the outcome at every later point.

Build the multi-service company world:

```bash
go run ./cmd/twinwright build-world examples/company/world.yaml \
  --out company.world.manifest.json
```

Run the deterministic incident scenario:

```bash
go run ./cmd/twinwright run company-incident \
  --agent scripted \
  --manifest company.world.manifest.json \
  --db company.db \
  --seed 42
```

The company scenario requires the agent to investigate customer state across services, determine whether a duplicate charge exists, perform the justified refund, update CRM, and escalate through ticketing and messaging when incident evidence requires it.
