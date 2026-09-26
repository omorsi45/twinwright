# Twinwright personal design workspace

This folder is ignored by Git and kept in Omar's local Twinwright checkout. Public architectural decisions remain in `docs/adr/`.

## Briefs

- `briefs/2026-09-24-initial-brief.md`: original WorldForge project brief, copied verbatim from the first attachment.
- `briefs/2026-09-25-platform-roadmap.md`: current Twinwright roadmap and definition of done, copied verbatim from the latest attachment. Read this first for long-term direction.

## Working designs

- `superpowers/specs/2026-09-24-billing-world-design.md`
- `superpowers/plans/2026-09-24-billing-world.md`
- `superpowers/specs/2026-09-25-replay-verification-design.md`
- `superpowers/plans/2026-09-25-replay-verification.md`
- `superpowers/specs/2026-09-25-company-world-design.md`: Milestone 2 stateful company world design.
- `superpowers/plans/2026-09-25-company-world.md`: Milestone 2 implementation plan.
- `superpowers/specs/2026-09-25-world-definition-design.md`: Milestone 3 versioned multi-service world definition design.
- `superpowers/plans/2026-09-25-world-definition.md`: Milestone 3 implementation plan.
- `superpowers/specs/2026-09-25-checkpoint-fork-design.md`: Milestone 4 checkpoint, restore, fork, and comparison design.
- `superpowers/plans/2026-09-25-checkpoint-fork.md`: Milestone 4 implementation plan, completed on PR 3.
- `superpowers/specs/2026-09-25-chaos-engine-design.md`: Milestone 5 deterministic fault and ambiguous-commit design.
- `superpowers/plans/2026-09-25-chaos-engine.md`: Milestone 5 implementation plan, completed on PR 4.
- `superpowers/specs/2026-09-25-principal-authorization-design.md`: Milestone 6 runtime identity, authorization, and adversarial security design.
- `superpowers/plans/2026-09-25-principal-authorization.md`: Milestone 6 implementation plan, completed on PR 5.
- `superpowers/specs/2026-09-25-assertion-framework-design.md`: Milestone 7 declarative assertion design.
- `superpowers/plans/2026-09-25-assertion-framework.md`: Milestone 7 implementation plan, completed on PR 6.
- `superpowers/specs/2026-09-25-counterfactual-debugger-design.md`: Milestone 8 intervention analysis and observation override design.
- `superpowers/plans/2026-09-25-counterfactual-debugger.md`: Milestone 8 implementation plan, completed on PR 7.
- `superpowers/specs/2026-09-25-observability-design.md`: Milestone 9 ledger traces, inspect summary, OTLP export, and redaction design.
- `superpowers/plans/2026-09-25-observability.md`: Milestone 9 implementation plan, completed on PR 8.
- `superpowers/specs/2026-09-25-providers-design.md`: Milestone 10 provider adapter design, completed on PR 9.
- `superpowers/plans/2026-09-25-providers.md`: Milestone 10 implementation plan, completed on PR 9.
- `superpowers/specs/2026-09-25-bench-design.md`: Milestone 11 Twinwright Bench design, completed on PR 10.
- `superpowers/plans/2026-09-25-bench.md`: Milestone 11 implementation plan, completed on PR 10.
- `superpowers/specs/2026-09-25-shadow-design.md`: Milestone 12 experimental shadow mode design.
- `superpowers/plans/2026-09-25-shadow.md`: Milestone 12 implementation plan.
- `progress.md`: current branch, PR, checks, limitations, and next work. Read this before continuing after compaction.

## Handoff

- Milestone 10: https://github.com/omorsi45/twinwright/pull/9. Milestone 11: https://github.com/omorsi45/twinwright/pull/10. Continue Milestone 12 from the shadow spec/plan.

The latest brief is a roadmap, not proof that its later milestones are implemented. Check the repository, open PRs, and public README for current behavior before starting the next milestone. Main contains Milestones 1 through 9 (`44d25e1`). Milestones 10 through 12 are open PRs (9, 10, 11). Milestones 13 and 14 have private design notes only. A live OpenAI run has not yet been verified with a key.

- superpowers/specs/2026-09-25-distributed-foundation-design.md / plans/2026-09-25-distributed-foundation.md — Milestone 14 lease fencing foundation (PR 13).
