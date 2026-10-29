# Twinwright guide

The project README is the Twinwright Cloud overview: what the platform is, how to run it, and what is and is not proven. This directory holds the detail that used to be in the README. Nothing was dropped: each page below says which README sections it holds, and the table shows where every old section went.

The documents that make claims are separate. [docs/claims.md](../claims.md) lists every claim with the test or CI job that proves it, and [docs/architecture.md](../architecture.md) has the component diagram, the path of one tool call, the trust boundaries and the index of all 34 architecture decision records.

## Where each old README section went

| Old README section | New location |
| --- | --- |
| Title, tagline and badges | [README.md](../../README.md) |
| Why Twinwright | [overview.md](overview.md) |
| What Twinwright does | [overview.md](overview.md) |
| Architecture | [docs/architecture.md](../architecture.md) |
| Quick start | [quick-start.md](quick-start.md) |
| Inspect and verify a run | [runs-and-replay.md](runs-and-replay.md) |
| Run traces | [runs-and-replay.md](runs-and-replay.md) |
| Declarative assertions | [worlds-and-assertions.md](worlds-and-assertions.md) |
| Checkpoints and counterfactual forks | [forks-and-counterfactuals.md](forks-and-counterfactuals.md) |
| Deterministic chaos policies | [chaos-and-authorization.md](chaos-and-authorization.md) |
| Principal authorization | [chaos-and-authorization.md](chaos-and-authorization.md) |
| Counterfactual analysis | [forks-and-counterfactuals.md](forks-and-counterfactuals.md) |
| Optional containers | [containers.md](containers.md) |
| Distributed runtime | [distributed-runtime.md](distributed-runtime.md) |
| Observability | [observability.md](observability.md) |
| Experimental shadow mode | [shadow-mode.md](shadow-mode.md) |
| Twinwright Bench | [bench.md](bench.md) |
| Agent providers | [providers.md](providers.md) |
| Engineering guarantees | [guarantees.md](guarantees.md) |
| Stateful company world | [worlds-and-assertions.md](worlds-and-assertions.md) |
| Defining worlds | [worlds-and-assertions.md](worlds-and-assertions.md) |
| Event model | [runs-and-replay.md](runs-and-replay.md) |
| Repository layout | [repository.md](repository.md) |
| Design philosophy | [overview.md](overview.md) |
| Building on Twinwright | [overview.md](overview.md) |
| Development | [development.md](development.md) |
| Architecture decisions | [repository.md](repository.md) |
| Safety and scope | [limitations.md](limitations.md) |
| Current limitations | [limitations.md](limitations.md) |
| License | [README.md](../../README.md) |

## Pages that gained material

Four pages end with sections that did not exist in the old README, because the README had no place for features added since: [providers.md](providers.md) (the output ceiling, rate limits and server errors, cassettes, a real model run, gating a run), [bench.md](bench.md) (the regression gate), [observability.md](observability.md) (worker export, metrics, the Compose stack, what CI shows) and [distributed-runtime.md](distributed-runtime.md) (what Compose starts, `make up` and `make down`). A few sentences were corrected where they had become false, and some were added to moved text; each page's opening paragraph lists them.

## Other documents

- [docs/distributed.md](../distributed.md): the fleet topology, the takeover sequence, the lease lifecycle and the crash boundary table.
- [docs/performance.md](../performance.md): measured figures, methodology and what is not measured.
- [docs/protocol/agent-v1.md](../protocol/agent-v1.md): the exec agent protocol.
- [docs/security-review.md](../security-review.md): the review pass over each internal boundary.
- [docs/live-model-run.md](../live-model-run.md): the one recorded run of a local model.
- [docs/adr/](../adr/): the architecture decision records.
