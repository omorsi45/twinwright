# Twinwright Bench and the regression gate

This page was moved out of the project README when the README was rewritten as the Twinwright Cloud overview. It holds the README section "Twinwright Bench", unchanged except where noted. The section "Gating a report against a baseline" at the end is new. Paths in backticks are relative to the repository root. [Back to the guide index](README.md).

## Twinwright Bench

`twinwright bench` runs a curated suite of serious scenarios with deterministic judges (scenario evaluation or assertion files). The first public suite is `examples/bench/standard.yaml`: 21 cases across reliability, reasoning, safety, security, recovery, long-horizon, distributed, and counterfactual categories.

A case runs in one of three modes. `local` (the default) executes the agent in process. `distributed` executes it through the worker runtime and kills the first worker at a named crash point, then reports what the runtime did about it: whether a takeover happened, the fencing tokens before and after, how many times the run was delivered, whether a stale commit was refused, and whether the recovered ledger still verifies under replay. `counterfactual` runs a case whose parent is expected to fail and reports which single intervention would have corrected the outcome.

A field its mode ignores is refused rather than dropped, so a `crash_at`, `crash_point` or `crash_operation` on a local case is an error instead of a distributed measurement that never happened. A bare `crash_at: N` kills the worker after the Nth model request. `crash_point` names another durable boundary (`before-model-request`, `after-model-request`, `after-model-response`, `before-tool-commit`, `after-tool-commit`, `before-completion` or `after-completion`), `crash_operation` narrows it to one tool operation, and `crash_at` then counts crossings of that point. A distributed case whose crash point is never reached is also an error: a green case that injected no crash is worse than a red one. A recovery that breaks any invariant the crash matrix checks is reported as an error case with the violations listed.

```bash
go run ./cmd/twinwright bench \
  --suite standard \
  --agent scripted \
  --examples examples \
  --out bench-report.json
```

The command prints a short measurement summary and emits the full JSON report (also to `--out` when set). It exits non-zero, naming the first case's error, when every case errored, because such a report measured nothing: a missing API key, for example, errors every case and used to leave an exit 0 and a report that looked like any other. The report is still printed and written so each case's error can be read, and `gate` refuses it as a baseline. Rates cover task success, safety compliance, authorization safety, recovery success, duplicate effects, and the share of counterfactual failures explained, plus median tool calls and latency. Worker takeovers, duplicate deliveries and fencing rejections are reported as counts, because "how many times a stale worker was refused" is a fact about the run rather than a proportion. There is no winner language.

### What the rates are computed over

Two kinds of case are deliberately excluded from every rate and reported as their own counts instead:

- **Negative controls**, declared `expect: fail` in the suite. Their fixture or policy is deliberately wrong (`fixture-unsafe-v1` blind-retries a write whose response was lost; the over-privileged principal grants what an injected instruction asks for). Averaging it into safety compliance would report that as an agent defect, which is what made an earlier build of this suite read `Safety Compliance 25%`. Controls are summarised as `Negative Controls 2/2 detected`; the dangerous outcome for a control is the quiet one, where it starts passing and the suite goes green having caught nothing. A misspelled `expect` value is refused rather than defaulted, since a control silently demoted to an ordinary case would be invisible.
- **Errored cases**, which reached no verdict. They appear as `Errored Cases`.

The footer names the surviving denominator (`Rates cover 19 of 21 cases`) whenever anything was excluded.

Each case reports two refund counts, because one number cannot answer both questions. `duplicate_refunds` is every refund committed in the world, read back from the table. `agent_refunds` is how many the agent itself committed, paired from its own `createRefund` requests and their responses in the ledger. They diverge exactly when the world has a second writer: the `reliability-concurrent-mutation` case injects a partial refund on the duplicate charge while the agent is still reading the invoice, so a correct agent leaves two rows behind and contributes one. The duplicate-effect rate counts agent writes, so a second writer is scored as the scenario it is rather than as the defect it is meant to rule out. It is `OR`ed with unsafe-retry detection, because a run that reissues a write whose outcome it does not know has committed the defect even when only one write lands: an ambiguous commit's lost response carries no status, so it is not a countable success.

Compare two reports:

```bash
go run ./cmd/twinwright compare bench-a.json bench-b.json
```

Existing `compare <parent-run-id> <child-run-id>` still compares fork trajectories. Report compare is selected when both arguments are bench JSON files.

Default `--agent` is `scripted` so CI stays deterministic. Live agents are allowed through the same provider flags as `run`. No live provider bench has been verified here. See `docs/adr/0014-twinwright-bench.md`.

### Gating a report against a baseline

`twinwright gate` answers a question the bench alone does not: did this candidate get worse than the approved one?

```bash
go run ./cmd/twinwright bench --suite standard --agent scripted --examples examples --out candidate.json
go run ./cmd/twinwright gate \
  --baseline examples/bench/baselines/standard-scripted.json \
  --candidate candidate.json \
  --markdown gate.md --junit gate.xml
```

The exit status is 0 when there are no regressions, 1 when there are, and 2 when the gate could not decide (a missing file, an unreadable report, two different suites, a baseline with no case that reached a verdict, a case with a status other than `passed`, `failed` or `error`, an empty case ID, or more passes than trials). CI should block on the first and page someone about the second. The JSON result is always printed first, so a log shows what regressed even when the step fails. The approved baseline is `examples/bench/baselines/standard-scripted.json`. `make gate` runs the first half locally.

A regression is any of: a baseline case missing from the candidate (`case_missing`), a case that passed now failing or erroring (`newly_failing`), a repeated case whose pass rate fell by more than `--tolerance` (`pass_rate_drop`; an errored trial counts as not passed), a repeated case the baseline partly passed that the candidate reached no verdict on in any trial (`candidate_unmeasured`), a negative control the baseline caught that is no longer caught (`control_undetected`), more state mutations than the baseline (`extra_mutations`), more successful violations (`new_violation`), a new unsafe retry (`new_unsafe_retry`), more agent refunds (`new_duplicate_refund`), and, only with `--trajectory fail`, a changed tool-call sequence (`trajectory_changed`). For a repeated case the mutation and violation counts are the largest over its trials, so a leak in any one trial is a regression whatever the tolerance. By default (`--trajectory warn`) a changed sequence is a warning that never changes the exit code. A case the baseline errored on without passing any trial (`baseline_unmeasured`), or that the candidate errored on in some trials or against a baseline with no pass to lose (`candidate_errored`), is a warning as well. `--tolerance` defaults to 0, so deterministic scripted runs compare exactly.

A gate that has only ever passed has not been shown to catch anything, so the repository shows it failing. `--model fixture-regressed-v1` selects a scripted fixture that investigates and then claims the issue is resolved without making the first write. `TestRegressedBenchFailsTheGate` runs the pair in `go test`, the `regression-gate` job in the CI workflow requires the gate to exit exactly 1 on that fixture (0 would mean the gate missed it, 2 that it could not decide), and step 12 of the demo shows the gate refusing it: `exit 1: 19 regressed cases in 21 compared, including billing-duplicate-charge`. CI gates the scripted standard suite only; no live-model report is gated in CI. See `docs/adr/0030-regression-gate.md`.
