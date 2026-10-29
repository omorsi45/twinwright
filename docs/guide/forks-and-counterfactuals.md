# Checkpoints, forks and counterfactual analysis

This page was moved out of the project README when the README was rewritten as the Twinwright Cloud overview. It holds the README sections "Checkpoints and counterfactual forks", "Counterfactual analysis", unchanged except where noted. One sentence in "Counterfactual analysis" about live models was corrected. Paths in backticks are relative to the repository root. [Back to the guide index](README.md).

## Checkpoints and counterfactual forks

Twinwright can reconstruct valid restore boundaries from a durable execution:

```bash
go run ./cmd/twinwright checkpoints <run-id> \
  --manifest company.world.manifest.json \
  --db company.db
```

Fork from a committed event boundary:

```bash
go run ./cmd/twinwright fork <run-id> \
  --at-event <event-seq> \
  --manifest company.world.manifest.json \
  --db company.db
```

The child receives an isolated world and explicit lineage back to the parent.

For example, continue with a different fault configuration or model:

```bash
go run ./cmd/twinwright fork <run-id> \
  --at-event <event-seq> \
  --fault createRefund \
  --steps 20 \
  --manifest company.world.manifest.json \
  --db company.db
```

Then compare the two trajectories:

```bash
go run ./cmd/twinwright compare <parent-run-id> <child-run-id> \
  --db company.db
```

## Counterfactual analysis

`counterfactual` asks which earlier events the failure depended on. It forks a completed, failed run at candidate events, changes exactly one controlled variable in each fork, executes the child with the run's provider, and judges it with the same success definition as the parent.

```bash
go run ./cmd/twinwright build examples/billing/openapi.yaml

go run ./cmd/twinwright run ambiguous-commit \
  --agent scripted \
  --recovery unsafe \
  --chaos examples/chaos/ambiguous-commit.yaml

go run ./cmd/twinwright counterfactual <run-id> \
  --interventions examples/counterfactual/ambiguous-commit.yaml \
  --trials 3
```

The JSON report ranks candidates by how many forks changed the outcome. Each candidate has a one-line summary:

```text
#4 createRefund dispatch [latency-instead-of-lost-response]: corrected the final outcome in 3/3 forks
#7 createRefund observation [refund-delivered]: corrected the final outcome in 3/3 forks
#9 model decision after createRefund [safe-recovery]: corrected the final outcome in 3/3 forks
#4 createRefund dispatch [extra-latency]: no material effect (0/3 forks corrected the outcome)
#10 createRefund dispatch [latency-instead-of-lost-response]: no material effect (0/3 forks corrected the outcome)
#14 model decision after createRefund [safe-recovery]: no material effect (0/3 forks corrected the outcome); 3 errored, 0 did not complete
```

Removing the lost response at the first refund, showing the agent a delivered response, or switching to the safe fixture after the timeout all prevent the duplicate refund. The same change at the retry comes too late. Every candidate lists its forks as evidence: run IDs with status and failed checks. Each fork is a normal child run that `replay`, `compare`, `inspect`, and `evaluate` accept.

| Kind | Changes | Fork point |
| --- | --- | --- |
| `chaos_policy` | Replaces the chaos policy | Before the targeted call is dispatched |
| `auth_policy` | Replaces the principal policy | Before the targeted call is dispatched |
| `fault` | Sets or clears the legacy one-time 503, on runs without a chaos policy | Before the targeted call is dispatched |
| `model` | Switches the provider or model for the next decision | After the targeted call's response |
| `tool_response` | Substitutes what the agent saw as one call's response | At that response |

`calls` limits an intervention to named call IDs; without it, every eligible call is a candidate. Everything is validated before the first fork is created. An intervention that would change nothing, such as the run's own model or fault, or two things at once, such as a chaos policy on a run with a legacy fault, is rejected. `--assertions` defines success; without it the scenario evaluation is used. The definition matters: `examples/assertions/ambiguous-commit.yaml` requires that the lost response was observed, so a fork that removes the fault fails it even though its refund is correct.

A `tool_response` fork changes only what the child saw: its transcript and saved result for that call. World state stays as it was. The substitution is recorded in `fork_observations` and as an `observation.overridden` event, so replay applies the same change and detects tampering with either. Assertions on such a fork still see the parent's original response in the inherited history; an `event_absent` assertion on `observation.overridden` excludes these forks from a success definition.

`examples/counterfactual/prompt-injection-ticket.yaml` does the same for the overprivileged prompt-injection run: the support policy corrects the outcome when applied at the ticket read or the foreign customer lookup, and has no effect once the lookup has already succeeded.

This is intervention analysis. The counts describe the forks that ran; they are not probabilities or proof of cause. Scripted fixtures make every trial identical. A live model can differ between trials, which is what `--trials` is for, but no counterfactual analysis has been run against a live model: the only live-model run recorded in this repository is the single `run` in `docs/live-model-run.md`. Only root runs can be analyzed, because forks of forks are unsupported, and the first model decision has no checkpoint before it. Direct world-state edits, memory, and execution strategy are not intervention kinds yet. Forks are written to the same database, and an analysis that aborts partway leaves the forks it already created.

See `examples/counterfactual/` and `docs/adr/0011-counterfactual-analysis.md`.
