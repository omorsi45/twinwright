# Milestone 8: counterfactual debugger plan

Spec: `design/superpowers/specs/2026-09-25-counterfactual-debugger-design.md`. Worktree `.worktrees/milestone-8-counterfactual`, branch `milestone-8-counterfactual` from `fe32d01`.

## Task 1: audited observation override on forks

- Files: `internal/store/store.go` (table `fork_observations`, `ObservationOverride` read with old-database fallback), `internal/fork/observation.go` (type, validation, transcript/result application, event payload), `internal/fork/fork.go` (option, apply in `Create`), `internal/fork/replay.go` (apply in `ReconstructForReplay`), `internal/replay/replay.go` (accept and compare `observation.overridden`).
- Tests: child transcript, saved result, row, and event carry the override; parent unchanged; rejection for non-tool-response checkpoint, other call, bad status, invalid body; completed override child replays; tampered row and tampered event diverge.
- Commit: `Add audited observation overrides to forks`

## Task 2: ambiguous fixture completes after a delivered refund

- Files: `internal/agent/ambiguous_scripted.go`, test.
- Tests: safe and unsafe fixtures finish after a 201 first refund; lost-response paths unchanged.
- Commit: `Finish ambiguous fixtures after a delivered refund`

## Task 3: intervention file parser

- Files: `internal/counterfactual/parse.go`, `parse_test.go`.
- Interface: `Parse(raw, manifest) (Set, error)`, `Set.Digest()`; kinds `chaos_policy`, `auth_policy`, `fault`, `model`, `tool_response`; inline policies validated with `chaos.Parse` and `authz.Parse`.
- Tests: each rejection case fails on its intended rule; digest stable across key order.
- Commit: `Parse counterfactual intervention files`

## Task 4: candidate discovery and analysis

- Files: `internal/counterfactual/analyze.go`, `judge.go`, `analyze_test.go`.
- Interface: `Analyze(ctx, source, destination, runID, manifest, set, judge, Options{Trials, Steps, ProviderFor}) (Report, error)`; `AssertionJudge(set)`, `ScenarioJudge()`.
- Tests: ambiguous unsafe run (chaos replacement at the lost-response call corrects, extra latency does not, observation override corrects, model switch corrects); prompt injection with overprivileged policy (support policy at the ticket read blocks the violation, late replacement does not); report deterministic apart from run IDs; parent unchanged; every completed fork replays; passing, paused, and forked parents rejected; unknown call rejected with no fork written.
- Commit: `Add counterfactual intervention analysis`

## Task 5: CLI, examples, documentation

- Files: `cmd/twinwright/main.go` + test, `examples/counterfactual/*.yaml`, `README.md`, `docs/adr/0011-counterfactual-analysis.md`.
- Tests: CLI end to end on the ambiguous unsafe run and the security run; missing flags and bad trials rejected.
- Commit: `Add counterfactual command and documentation`
