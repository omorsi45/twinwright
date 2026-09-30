# 0025. Honest bench aggregation: negative controls, errored cases, and writer attribution

Date: 2026-09-29

Status: accepted

## Context

Running the shipped `standard` suite produced this headline:

```
Task Success              84.2%
Safety Compliance         25.0%
```

Both figures were wrong in a way that mattered more than being merely imprecise: a reader's
reasonable conclusion from `Safety Compliance 25%` is that Twinwright's agent runtime is unsafe
three times out of four. Nothing in the report contradicted that reading, so the benchmark was
actively misrepresenting the project it exists to measure.

Three distinct conflations produced it.

**Negative controls were scored as agent behaviour.** `safety-ambiguous-unsafe` runs
`fixture-unsafe-v1`, which blind-retries a write whose response was lost, and
`security-injection-overprivileged` runs a principal deliberately granted what the injected
instruction asks for. Both are *supposed* to fail. Their failure is the suite proving it still
catches a known-bad behaviour. Averaged into safety compliance, the benchmark's own success was
reported as the agent's defect, and those two cases are the entire 25%.

**An errored case was scored as a failure on every dimension it declared.**
`reliability-concurrent-mutation` stopped at `fixture cannot continue after createRefund HTTP 409`
and therefore measured nothing at all, yet it counted against task, safety and duplicate effects.
A stalled harness was being reported as three agent failures.

**The duplicate-effect metric counted refund rows rather than agent writes.** That silently assumes
the agent is the only writer in the world, which is true of every duplicate-charge case except the
one built to break it.

## Decision

### Expectation is declared, not inferred

A case may declare `expect: fail`. The default is `pass`, and any other value is refused at parse
time rather than defaulted, because a control silently demoted to an ordinary case is exactly the
conflation being removed and would be invisible in the report. The alternative considered was
inferring control status from the case id or the fixture name (`*-unsafe`, `fixture-unsafe-v1`).
Rejected: the suite would then decide semantics by string matching, and renaming a case would
silently change what the benchmark claims.

Controls are excluded from every rate and summarised as `Negative Controls 2/2 detected`. Each case
also carries `as_expected`, so a reader can tell "failed, as intended" from "failed, unexpectedly"
without knowing which fixtures are deliberately broken. The reported failure mode for a control is
the quiet one: a control that starts *passing* surfaces as undetected rather than as a green case.

### Errored cases are excluded and counted

A case that reached no verdict leaves every rate denominator, and the count appears as
`Errored Cases`. It contributes no latency or tool-call median either, since it stopped at whatever
step broke. Whenever anything was excluded the footer names the surviving denominator: a percentage
over a reduced denominator is honest only if the reader is told the denominator moved.

### The errored case was fixed in the agent, not in the checks

The 409 was real. billing refuses a refund exceeding the remaining charge, and the concurrent rule
commits a 500-cent refund on the duplicate while the agent is still listing charges, so the amount
the agent computed from the invoice is stale by the time it writes. The correct behaviour is to
re-read the charge and refund only what is still outstanding, which the billing-world
ambiguous-commit fixture already did. Retrying the same amount fails identically; refunding blind
would exceed the charge.

The built-in `scenario_evaluation` checks were **not** weakened to accommodate this.
`exactly_one_refund` and `correct_charge_identified` count refund rows, and those same row counts are
what detects the genuine double refund in the ambiguous-commit cases. Instead the concurrent case
gets its own assertion file stating what matters under a second writer: the duplicate refunded
exactly its own amount, one concurrent write plus exactly one agent write, the conflict observed,
and reconciliation ordered *after* the conflict rather than merely present.

### Writer attribution for duplicate effects

`CaseResult` now carries both counts. `duplicate_refunds` is every refund in the world, read back
from the table. `agent_refunds` is what the agent committed, paired from its own `createRefund`
requests and their responses in the ledger. A chaos actor's write is recorded as
`chaos.actor_mutation` rather than as a `tool.request`, which is what makes the two writers
separable without heuristics.

The duplicate-effect rate uses `agent_refunds`, `OR`ed with unsafe-retry detection. The `OR` is
load-bearing: an ambiguous commit's lost response carries no status, so the write it may have
committed is not a countable success, and `agent_refunds` alone would read 1 for a run that
reissued the refund. `safety-ambiguous-unsafe` demonstrates exactly that shape
(`agent_refunds: 1, unsafe_retry: true`), and it is the unsafe-retry term that catches it.

Both numbers are reported rather than one derived number, because "two refunds exist and the agent
wrote one" is the fact that makes the concurrent case legible.

### A dimension no case declared is labelled, not printed as zero

A rate over an empty denominator is not zero, it is absent, and the two read as opposites. The
flagship distributed suite is the first shipped suite that declares a subset of the dimensions, and
it printed `Safety Compliance 0.0%` and `Authorization Safety 0.0%`: a reader scanning that concludes
the agent failed every safety case, when the truth is that the suite contains none. This is the same
defect as the original 25% figure, reached from the opposite direction, and any suite covering a
subset of the dimensions hits it.

`Summary.Measured` now carries the denominator behind every rate, keyed by dimension name, and
`FormatText` prints `not measured` in place of a percentage when that denominator is zero. The
machine-readable summary keeps the numeric field, so a consumer reading JSON is not forced to parse
prose; the denominator beside it is what distinguishes zero-of-zero from zero-of-many.

Silently omitting the line was the alternative, and it is weaker: a reader who does not see Safety
Compliance at all cannot tell whether the report predates the dimension or the suite skipped it.
Naming the absence says which.

## Consequences

The shipped suite now reports task success, safety compliance, authorization safety and recovery
success at 100%, duplicate effects at 0%, `2/2` controls detected, no errored cases, and rates
covering 18 of 20 cases. None of those numbers moved because a check was relaxed: the two excluded
cases are excluded for a stated reason, and the twenty-case suite still fails if either control
stops being caught.

The concurrency dimension of section 8 is now a measurement rather than a crash. The scenario it
measures, an agent reconciling against a concurrent partial write instead of retrying blind, is a
harder and more realistic behaviour than the case asserted before.

Mutation evidence for each claim:

- Restoring `duplicate_refunds > 1` in the rate makes `TestConcurrentWriterIsNotADuplicateEffect`
  report 100% duplicate effects.
- Replacing the remainder computation with the full charge amount makes two fixture tests fail, one
  reporting the fixture re-refunding 5905 on a charge already partly refunded.
- Removing the exclusions makes the control and errored-case tests fail on the rates directly.
- Forcing the unmeasured branch off makes `TestUnmeasuredDimensionIsNotRenderedAsZeroPercent` report
  `Safety Compliance 0.0%` on a suite that declared no safety case.

## Rejected alternatives

**Report only a single overall pass rate.** Simpler, and it would have hidden the problem rather than
fixed it: a reader cannot tell a deliberate control from a regression in one number.

**Drop the negative controls.** They are the only evidence the suite's judges still work. A benchmark
whose checks have quietly stopped firing reports perfect scores.

**Give the concurrent case `expect: fail`.** It would have gone green with one line and measured
nothing, which is the same defect as the errored case wearing a different label.
