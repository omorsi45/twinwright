# 0026. Shadow comparison depth: semantic, timing and policy divergence

Date: 2026-10-02

Status: accepted

## Context

The shadow comparison keyed an action on its operation and the whole of its marshalled
arguments, and reported three lists: matched, only-proposed, only-observed. That is an
exact byte comparison, and it fails on the case the feature exists for.

A proposed refund of 500 against an observed refund of 5905 *on the same charge* came
back as two entries:

```
only_proposed: [createRefund]
only_observed: [createRefund]
```

Both halves are useless alone. The reader has to notice that they are the same charge
and reassemble the finding by hand, and nothing in the report says they are related.
The one thing a shadow run is for - "the agent would have done something different
here, and here is what" - was the one thing the report could not express.

Two further divergences were absent entirely. The package contained no reference to
timing, and no reference to the authorization policy: identical actions in a different
order were reported as cleanly matched, and a proposed call the principal's policy
would refuse was reported as a match whenever a human had performed the same call.

## Decision

Three classifiers, all deterministic, none of them declaring a winner.

**Argument divergence.** Pair the actions left over after exact matching when they are
the same operation on the same resource, and report the fields that differ with both
values. Resource identity comes from the arguments that name the resource: an argument
called `id` or ending in `_id` carrying a non-empty string. Pairing on the operation
alone was rejected: it would call a refund of `CH-2` and a refund of `CH-9` the same
action and report a divergence on `charge_id`, which reads as a different amount when
it is a different charge. Pairing by position was rejected for the same class of
reason: a call site added on one side renumbers everything after it.

A call carrying no identifying argument is never paired on resource. There is nothing
to establish that two of them address the same thing, and a wrong pair is worse than an
unpaired residual.

**Timing.** Report the pairs the two streams sequence differently. Order is the timing
property both sides carry. Elapsed time is not: the proposed side comes from a local
simulation whose wall clock has no relationship to how long a human took, so a gap
comparison would be noise in a measurement's clothing. The report says that in
`timing.elapsed_gap` rather than printing `0`, and carries
`timing.observed_timestamps` beside `observed_count` so a reader can see how much
timing information the observed stream had at all. A rate over an empty denominator is
absent, not zero.

**Policy.** Screen each proposed action against the configured principal policy and
report the refusals. A refusal stands even when the observed stream contains the same
action: what a human with other permissions did is not evidence that this principal may
do it, and the `also_observed` flag records the overlap without softening the finding.

The screen reuses the enforcement path rather than reimplementing it. `authz.Screen`
now holds the whole decision and `authz.Decide` is `Screen` plus the two things only a
live run supplies - the stored policy and the next call number - so the dispatcher and
the shadow report cannot disagree about what the policy says. A second copy of those
rules would keep passing its own tests while drifting away from the enforced ones.
Screening happens inside `Simulate` because an accurate verdict needs the world the
calls would have landed in: customer scope is resolved by following that world's own
relationships. It reads; it writes nothing and executes nothing.

When no policy is configured, the report carries `policy.evaluated: false` and a
sentence saying why. An empty refusal list on its own reads exactly like a clean
screen.

## Consequences

Shadow mode stays observe-only. There is no new write path, nothing is dispatched, and
the config still rejects `allow_writes` and any mode but `observe`.

Output stays deterministic and sorted, which `internal/shadow/determinism_test.go`
holds over 200 repetitions, and the report still declares no winner, which
`TestCompareNoWinnerLanguage` holds. `Compare` remains a pure function of its two
streams; the policy screen is passed in through `CompareOptions` rather than given a
database handle.

`Simulate` now returns a `Result` rather than a pair of values, because the policy
screen is a third thing a simulation produces.

Order divergences are reported pairwise, so a fully reversed stream of n paired actions
yields n(n-1)/2 entries. For the stream lengths a shadow run produces that is the
complete answer rather than a sampled one; a cap would be the wrong trade for evidence.

What this still cannot do: identify a resource that is addressed by a non-string key, or
pair two calls that name no resource at all. Both are left as residuals, visible in the
only-lists, rather than guessed at.
