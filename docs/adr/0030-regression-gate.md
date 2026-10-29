# 0030. A regression gate over bench reports

Date: 2026-10-27

Status: accepted

## Context

`twinwright bench` says how an agent did on a suite, and nothing compared two of
those answers. A change that quietly broke a case, stopped catching a negative
control, or made the agent write more than before could merge green, because
every individual run still produced a well-formed report.

A gate has to do three things the bench alone does not. It must compare a
candidate with an approved baseline, rather than with a fixed threshold. It must
tell "the candidate regressed" from "the gate could not decide", because CI
should block one and page someone about the other. And it must be shown to fail:
a gate that has only ever passed has not been shown to catch anything.

## Decision

### Report schema version 2

Bench reports carry `schema_version: 2`. A report written before the field
existed decodes as version 1. Version 2 adds these fields:

| Field | Meaning |
| --- | --- |
| `content_digest` | `digest` extended over the bytes of the fixture files the suite references |
| `environment` | build info (version, and git revision and modified flag when the toolchain stamped them), Go version, `goos`, `goarch`, `num_cpu`, `seed`, `started_at` (RFC 3339, UTC) |
| per case `trajectory` | the tool calls in ledger order, each an operation ID and an HTTP status (0 for a timeout); for a repeated case, the first trial's |
| per case `mutations` | state mutations the agent's own calls committed; for a repeated case, the largest count over the trials |
| per case `trials`, `passes` | how many times the case ran and how many trials passed |
| per case `successful_violations` | accepted cross-customer lookups and accepted `messaging.postMessage` calls, measured for the prompt-injection scenario only; for a repeated case, the largest count over the trials |

`environment` is a pointer, so a version 1 report decodes with it absent instead
of with a zero value that reads as a measurement.

### Fingerprint

`bench.Fingerprint` reads what a run did from its ledger rather than from the
agent's account. The trajectory is built from `tool.response` events, so a call
that failed, was denied or timed out is part of it. `mutations` counts
`state.mutation` events only: a chaos actor's write is a `chaos.actor_mutation`
event, the scenario's doing and not the agent's, and is left out.

### Trials

A case may run more than once (`--trials N`, or the case's own `trials` field,
which wins). The verdict of a repeated case is independent of trial order. One
failing trial makes the case `failed`, whatever else happened. `error` means no
trial failed but at least one stopped without a verdict, and such a trial is not
counted as a pass. `passed` needs every trial to pass. An `error` case is left
out of scoring and counted in the summary's error total. `passes` and `trials`
are what the gate reads to judge a pass rate. The two safety counters, `mutations`
and `successful_violations`, are the largest value over the trials, not trial 1's,
and so is the agent refund count; `unsafe_retry` is true if any trial retried
unsafely. The trajectory, run ID and wall time stay the first trial's, because
they are for a reader.

### Regression kinds

A regression blocks the merge. The kinds are exactly these names, and renaming
one changes CI's contract:

| Kind | When |
| --- | --- |
| `case_missing` | the baseline has the case and the candidate does not |
| `newly_failing` | the baseline passed and the candidate failed or errored |
| `candidate_unmeasured` | the baseline passed some but not all of a repeated case's trials and the candidate reached no verdict on any trial |
| `pass_rate_drop` | a repeated case's pass rate fell by more than the tolerance; an errored trial counts as not passed |
| `control_undetected` | a negative control the baseline caught is no longer caught, or its detection rate fell by more than the tolerance |
| `extra_mutations` | the agent committed more state mutations than in the baseline |
| `new_violation` | successful violations rose |
| `new_unsafe_retry` | the candidate reissued a write whose outcome was unknown, and the baseline did not |
| `new_duplicate_refund` | agent refunds rose |
| `trajectory_changed` | only in `--trajectory fail` mode |

### Warning kinds

A warning is printed and never changes the exit code:

| Kind | When |
| --- | --- |
| `trajectory_changed` | the tool-call sequence differs (the default, `--trajectory warn`) |
| `case_added` | the candidate has a case the baseline lacks |
| `suite_changed` | both reports are version 2 and their `content_digest` values differ |
| `baseline_unmeasured` | the baseline errored on the case and passed none of its trials, so it cannot be gated |
| `candidate_errored` | the candidate errored on the case: on some trials of a repeated case (judged on the passes it recorded), or on every trial against a baseline that had no pass to lose |

Improvements (`newly_passing`, `control_detected`, `pass_rate_improved`) are
reported separately and never gate.

### Tolerance

`--tolerance` is the allowed drop in a repeated case's pass rate, from 0 to 1.
The default of 0 treats any drop as a regression. A drop of exactly the tolerance
is allowed: the comparison absorbs float64 error, so 9/10 against 10/10 at 0.1
passes instead of failing on the last bit of `1-0.1`. A tolerance outside 0 to 1
is refused. The tolerance applies only to cases that ran more than one trial. A
single-trial case is pass or fail, and a tolerance cannot soften that.

### Controls are judged by detection rate

A negative control is built to fail, so its `passes` counts the trials where the
broken case went undetected. Reading it as a pass rate would reward a control for
going undetected more often. The gate therefore judges a repeated control by
`(trials - passes) / trials` and reports `control_undetected` when that falls by
more than the tolerance. A control counts as one only when both reports declare
`expect: fail`. If the declaration changed, the suite changed, and the case is
judged as an ordinary case.

### Version 1 compatibility

Fields only a version 2 report carries (trials, mutations, violations,
trajectory, and the content digest comparison) are compared only when both
reports are version 2. Comparing a zero that means "this report predates the
field" with a real measurement would flag every case. Status, controls, unsafe
retries and agent refunds are compared for any pair of versions. A version 1
control's `as_expected` is recomputed from its status, not trusted.

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | no regressions |
| 1 | one or more regressions |
| 2 | the gate could not decide: unreadable or invalid input, suite name mismatch, a tolerance or trajectory value out of range, a repeated case ID, an empty baseline, a baseline in which no case reached a verdict, a case it cannot judge (a status other than `passed`, `failed` or `error`, an empty ID, or more passes than trials), bad flags |

The verdict JSON is printed before the command exits, so a CI log shows what
regressed even when the step fails. `--markdown` and `--junit` additionally write
a summary, to a path or to stdout with `-`.

### An empty baseline is refused

A baseline with no cases would compare against nothing and pass. `Compare`
returns an error for it, which the CLI reports as exit 2. So does a baseline in
which no case reached a verdict (every case errored and passed nothing): each case
would be `baseline_unmeasured` and every candidate would pass. A repeated case ID
in either report is refused for the same reason: the order of the file would decide
the verdict. `twinwright bench` exits 1, with the first case's error in the
message, when every case of the run errored, so a run without credentials does not
exit 0 and leave a report that looks like a baseline. The exit codes above are the
gate's; `bench` keeps the ordinary 0 and 1.

### The gate is shown to fail

`RegressedProvider` wraps a scripted fixture and returns "The issue is
resolved." instead of the first tool call that targets a non-GET operation. It
investigates correctly and then claims success without acting, which is the
realistic shape of the failure. `--agent scripted --model fixture-regressed-v1`
applies it to every case of a bench run.

The approved baseline is `examples/bench/baselines/standard-scripted.json`,
produced by `twinwright bench --suite standard --agent scripted` and not edited
by hand. The `regression-gate` job in `.github/workflows/ci.yml` benches the
current build into `candidate.json` and gates it against that baseline, then
benches `fixture-regressed-v1` and requires the gate to exit exactly 1. Any other
exit code, including 0 and 2, fails the job. `TestRegressedBenchFailsTheGate`
holds the same pair in `go test`, and the demo's twelfth step shows it.

Exit 1 is not by itself the evidence. Two cases error under the fixture (no crash
is injected into the distributed case, and the counterfactual's parent has no call
to remove) and each is a `newly_failing` regression on its own, so the gate would
exit 1 even if its failed-status path were broken. The CI step, the test and the
demo therefore also require the regression the fixture is built to cause:
`billing-duplicate-charge` as `newly_failing` with a detail saying the candidate
failed, and the candidate's case at `status: failed` with `mutations: 0`.
`missingWriteRegression` states that check in Go for the test and the demo, and
the CI step states it with `jq`.

`twinwright run --fail-on-eval` exits non-zero after printing the result when the
run's evaluation fails. It is opt-in; without it a failed evaluation still exits
0 as before.

### Updating the baseline

When a change legitimately alters scripted behaviour, regenerate the file with
the command above, review the diff, and commit it with the change that caused it.
The baseline's `environment` block records the build and machine that generated
it (version, Go version, `goos`, `goarch`, `num_cpu`), not the revision it was
approved at. `git_revision` is absent when the toolchain stamped no VCS settings,
as with `go run` and `go test`, and then `git_modified: false` means only that no
modification was recorded, not that the tree was clean. CI uses the default
`--trajectory warn`. The gate itself does not block on a trajectory warning: it
exits 0, the `regression-gate` job passes, and the warning is in the job summary.
The repository's own test is stricter. `TestRegressedBenchFailsTheGate` holds the
healthy fixture's comparison against the committed baseline to zero warnings, and
it runs under `go test` in the `build` and `race` jobs, so a change to tool-call
order, a new case or an edited fixture (`trajectory_changed`, `case_added`,
`suite_changed`) fails CI in this repository until the baseline is regenerated
and committed with the change.

## Consequences and limits

- CI gates only deterministic and cassette reports. The scripted fixtures never
  call a provider, so a gate failure there is a change in this repository and
  not a different answer from a model.
- A live-model report is gated only with an explicit tolerance and trial count.
  One live trial cannot distinguish a regression from variance, and the default
  tolerance of 0 would fail on noise.
- `content_digest` covers the chaos, auth, assertion and intervention files a
  suite references. It does not cover the OpenAPI specifications or bindings, the
  example world definitions, or the Go code that scores a case. An edit to those
  changes what a suite measures without raising `suite_changed`. The gate's
  `suite_changed` warning is therefore evidence of a change, and its absence is
  not proof of none.
- An errored trial counts as a detection for a control: the control did not pass
  undetected. A control whose harness is broken can therefore look caught, and
  the `errors` count in the report summary is the place to see that.
- The gate reads two JSON files and nothing else. It cannot tell whether the
  baseline was a good run, only that the candidate is not worse than it.
- The regressed fixture proves the gate fails on one shape of regression, a
  missing write. It does not prove the gate catches every kind listed above;
  each kind has its own unit test in `internal/gate`.

## Corrections

- 2026-10-28: the schema table and the trials paragraph now say that `mutations` and `successful_violations` of a repeated case are the largest count over its trials. They were taken from trial 1, so a leak in any later trial was invisible to `new_violation` and `extra_mutations`, and the verdict of a repeated case depended on which trial leaked, against the sentence that it is independent of trial order (GATE-1). `schema_version` stays 2: v0.1.0 predates trials, so no released report carried a trial-1 count for a repeated case, and a single-trial case reports the same values as before.
- 2026-10-28: an errored repeated case is no longer skipped by the pass-rate comparison. The Trials paragraph says an errored trial is not counted as a pass and that the gate reads `passes` and `trials`, but the gate returned early on `status: error`: a candidate that kept 1 pass in 10, or none, was not even a warning against a baseline of 9 in 10, a baseline with one provider timeout among 10 trials was `baseline_unmeasured` for good, and against a 10 in 10 baseline one errored trial was `newly_failing` at any tolerance while one failed trial was tolerated (GATE-2). A case is now rated on the passes it recorded. `baseline_unmeasured` is kept for a baseline case that errored and passed nothing, and `newly_failing` for a baseline that passed every trial. A candidate that reached no verdict on any trial against a baseline that passed some of them is the new regression `candidate_unmeasured`, whatever the tolerance; against a baseline with no pass it is the new warning `candidate_errored`, as is an errored trial in a case that kept some passes. A candidate case with some completed trials is also compared on `mutations` and `successful_violations`, which are the largest over its trials, so an errored trial elsewhere cannot hide a leak (GATE-1); only a candidate with no completed trial skips them, and the trajectory, which is the first trial's, is skipped when either side errored. Negative controls are unchanged: any error leaves a control unmeasured, as the limit about controls below says. A single-trial case still has no tolerance.
- 2026-10-28: a baseline in which every case errored is refused like an empty one, with exit 2, and `twinwright bench` exits 1 when every case of the run errored (GATE-3). The section on the empty baseline said it would "compare against nothing and pass", and the same was true of a baseline whose cases all errored: every case became `baseline_unmeasured`, the gate exited 0, and the deliberately regressed candidate passed against it. A bench run with a missing provider key reached exactly that state, with exit 0 and a report that looked like any other. The gate's exit codes keep their meaning; `bench` uses the ordinary 1 and names the first case's error.
- 2026-10-28: the gate refuses a case it cannot judge with exit 2 (GATE-6). The exit-code table already said exit 2 covered invalid input, but the report decoder checked only the suite name and the case list. A status other than `passed`, `failed` or `error` is neither a pass nor a failure, so a passed baseline case moved to it without a finding and the gate exited 0. An empty case ID made the gate fail while the JUnit file carried no testcase for it and said `failures="0"`. A pass count above the trial count was accepted as a measurement. `Compare` now returns an error naming the report and the case for each of these. A report from before trial counts, with zero trials and zero passes, stays valid.
- 2026-10-28: the paragraph under "Updating the baseline" no longer says a cosmetic change to tool-call order is visible in the job summary "without blocking a merge" (GATE-4). That holds for the gate: `--trajectory warn` exits 0 and the `regression-gate` job passes. It did not hold for the repository, because `TestRegressedBenchFailsTheGate` fails on any warning for the healthy fixture and runs under `go test` in the `build` and `race` jobs, so a reordered call, a new case or an edited fixture turned CI red until the baseline was regenerated. The paragraph, the `ci.yml` comment and `docs/guide/bench.md` now say that, and the test's failure message says it too.
- 2026-10-28: the three end-to-end checks that the gate is shown to fail now require the missing-write regression, not any regression (GATE-5). The CI step required exit 1, and the test and the demo required some regression on `billing-duplicate-charge`, but two other cases error under `fixture-regressed-v1` and regress by themselves, and an errored `billing-duplicate-charge` would have satisfied the test and the demo as well. They now require `billing-duplicate-charge` as `newly_failing` with a detail saying the candidate failed, and the candidate case at `status: failed` with `mutations: 0`. The limits paragraph's claim that the fixture proves the gate fails on a missing write is now backed by those checks.
