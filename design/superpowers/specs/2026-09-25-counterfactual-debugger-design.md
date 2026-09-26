# Milestone 8: counterfactual debugger design

Roadmap section: `briefs/2026-09-25-platform-roadmap.md`, "MILESTONE 8: COUNTERFACTUAL DEBUGGER" (line 860).

## Intent

For a completed run that fails its success definition, find which earlier ledger events are sensitive: fork at each candidate point, change exactly one controlled variable, execute the child to completion, evaluate it with the same definition, and report how often the change flipped the outcome. The method is intervention analysis (counterfactual sensitivity). It is not formal causal inference: it measures what one intervention did in N forks, with fork run IDs as evidence.

## Existing pieces

- `checkpoint.List` gives committed boundaries (model responses, tool responses, pause, completion) on root runs only.
- `fork.Create` builds an isolated child world and run at a checkpoint, with options for provider/model, legacy fault, replacement chaos policy, and replacement authorization policy. Replacements are recorded in `fork_lineage`.
- `fork.ReconstructForReplay` + `replay.Verify` verify a completed child from the parent's prefix plus its own recorded turns.
- `assertion.Parse/Check` define success for any run; `eval.Evaluate` is the scenario fallback.
- Scripted fixtures give stable outcomes; a live model would not.

## Approaches considered

1. Replay-only counterfactuals: re-run the parent's recorded assistant turns after an intervention. Rejected: recorded turns were chosen in response to the original observations, so replaying them after a change measures nothing about the agent.
2. Fork and execute with the run's provider (chosen). Each intervention is one fork option; the child runs live (or scripted) downstream and is evaluated independently. Repeated trials matter only for nondeterministic providers.
3. Direct world-state edits at a checkpoint (change a CRM row). Rejected for now: arbitrary state edits need a new audited mutation path and would make "retrieved record" ambiguous. A changed retrieved record is modeled as an override of the read's observation.

## Intervention kinds (version 1)

Each intervention changes one variable and names which parent calls it targets (`calls`, default every eligible call).

| Kind | Fork checkpoint | Altered event |
| --- | --- | --- |
| `chaos_policy` (inline policy) | boundary before the call's `tool.request` | the call dispatch |
| `auth_policy` (inline policy) | boundary before the call's `tool.request` | the call dispatch |
| `fault` (legacy operation or empty to clear) | boundary before the call's `tool.request` | the call dispatch |
| `model` (provider, model) | the call's `tool.response`, only when a model request follows | the next `model.response` |
| `tool_response` (call, status, body) | the call's `tool.response` | that observation |

The boundary before any `tool.request` is always a checkpoint (it follows a model response or a previous tool response). The first model decision has no checkpoint before it, so it is not a candidate. `fault` is rejected on runs that carry a chaos policy, because a legacy fault and a chaos policy cannot be combined.

## Observation override (new fork option)

`fork.Options.Observation{CallID, Status, Body}` substitutes the saved observation of the call whose `tool.response` is the fork checkpoint, in the child only:

- Validation: the checkpoint event must be that call's `tool.response`; status is 0 or 100 to 599; the body is one JSON value, stored canonically.
- The child's last transcript message (the tool message for that call) gets the new content and status, and the child's copy of `tool_results` for that call gets the same status and body, so a reused call ID in the child returns what the child saw.
- A `fork_observations(child_run_id, call_id, status, body)` row is inserted and an `observation.overridden` event (call ID, operation, original status, new status, new body) is appended right after `execution.forked`, in the same transaction.
- World state is not changed; the override deliberately makes the observation disagree with the world, which is the point of the intervention.
- Replay: `ReconstructForReplay` reads the row and applies the identical change and event; `observation.overridden` is a semantic event in replay comparison. Deleting or editing either the row or the event diverges. Databases without the table read as having no override.
- Assertions on a child still see the parent prefix, including the original `tool.response`; the override is its own event.

## Analysis

`counterfactual.Analyze(ctx, source, destination, runID, manifest, set, judge, options)`:

1. Parent must be a root run (`execution.started` first), completed, and fail the judge. Otherwise it is rejected explicitly.
2. Discover calls from the parent ledger: call ID, operation, request seq, response seq, and the next decision seq.
3. Expand interventions to candidates; an unknown call ID or ineligible target is an error. Validate every fork option and resolve every provider before creating any fork, so configuration errors write nothing.
4. For each candidate and trial: `fork.Create`, run the child with the selected provider up to `steps`, then judge it. A runner error records the fork as errored (its run stays `failed` in the database). A paused child is incomplete. Neither counts as a change.
5. Rank by changed/forks descending, then changed, event seq, intervention ID. Each candidate carries a one-line summary and fork evidence (run ID, status, passed, failed checks, error).

Judge: an assertion set (`assertion.Check`) when `--assertions` is given, else the scenario evaluator.

## CLI

`twinwright counterfactual <run-id> --interventions <file> [--assertions <file>] [--trials N] [--steps N] [--manifest path] [--db path]`. The source is opened read-only for reconstruction; forks are written to the same database file. Output is JSON. Trials 1 to 100.

## Fixture change

`AmbiguousScriptedProvider` currently errors when the first refund returns 201. It will finish with a confirmation instead, so a fork that removes the lost response can complete. Only paths that errored before change; recorded runs and replay are unaffected.

## Limits (documented)

- Counts over N forks, not probabilities or causal claims. Scripted fixtures make every trial identical; live models were not exercised (no API key).
- Forks of forks are unsupported, so candidates come only from root runs.
- No direct world-state edits, memory items, or execution strategy variables yet.
- An aborted analysis may leave already created forks in the database; they are isolated children with lineage.
