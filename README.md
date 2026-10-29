<div align="center">

# Twinwright Cloud

**A self-hosted platform for testing autonomous AI agents against stateful simulated worlds, with a regression gate, a worker fleet that survives crashes, and traces and metrics a real collector can ingest.**

[![CI](https://github.com/omorsi45/twinwright/actions/workflows/ci.yml/badge.svg)](https://github.com/omorsi45/twinwright/actions/workflows/ci.yml)
[![Integration](https://github.com/omorsi45/twinwright/actions/workflows/integration.yml/badge.svg)](https://github.com/omorsi45/twinwright/actions/workflows/integration.yml)
![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go&logoColor=white)
![License](https://img.shields.io/badge/License-Apache--2.0-blue)
![Focus](https://img.shields.io/badge/Focus-Agent%20Infrastructure-black)

</div>

---

## What it is

Autonomous agents call tools, change business state, retry failed operations and carry on after partial failures. A mock returns canned answers. Twinwright runs the agent against simulated software that keeps state: billing, CRM, ticketing and messaging services over one database, with injected faults and runtime permissions. It records every model turn and tool call in an ordered ledger, decides success by checking the world's final state rather than the agent's account of itself, and can replay, fork and compare the run afterwards.

"Twinwright Cloud" names the self-hosted platform built on that runtime: a fleet of workers over one PostgreSQL database, a regression gate that fails CI when an agent gets worse, provider recording, a protocol for testing an agent written in any language, traces and metrics for a real collector and Prometheus, and published benchmarks. **It is not a hosted service.** There are no accounts, no tenants and no web UI, and there is no HTTP API for submitting runs: the CLI and the PostgreSQL queue are the interface.

What a run can do. Every line is held by a test or a CI job named in [docs/claims.md](docs/claims.md).

- Commit a tool call's effect, its ledger events and its saved result in one transaction, and make a repeated call ID idempotent. The same call ID with different arguments is refused.
- Inject deterministic faults: errors, timeouts, a write that commits and then loses its response, stale reads. See [chaos and authorization](docs/guide/chaos-and-authorization.md).
- Run the agent as a principal whose permissions are enforced in code inside the tool transaction, and report blocked prompt-injection attempts separately from task success.
- Replay a completed run in an isolated world with no model call, fork it at a checkpoint, and rank which events a failure depended on. See [forks and counterfactuals](docs/guide/forks-and-counterfactuals.md) and [runs and replay](docs/guide/runs-and-replay.md).
- Check a run against declarative assertions on the final state and the order of events. See [worlds and assertions](docs/guide/worlds-and-assertions.md).
- Run several workers that own a run under a fenced lease, take over a crashed worker's run, and refuse a commit from a worker that was fenced out. Delivery is at-least-once, never exactly-once.
- Compare a bench report with an approved baseline and fail CI on a regression.
- Test an agent of your own through a versioned protocol, optionally inside a hardened container.

Experimental and narrower: [shadow mode](docs/guide/shadow-mode.md) (observe-only, recorded files only) and [optional container sidecars](docs/guide/containers.md). The reasoning behind the design is in [overview](docs/guide/overview.md), [guarantees](docs/guide/guarantees.md) and [docs/architecture.md](docs/architecture.md), which has the component diagram, the path of one tool call, the trust boundaries and an index of all 34 architecture decision records.

## Quick start

Two commands show the platform. Neither needs an API key.

### `make demo`: one process, Go only

```bash
make demo
```

Needs Go 1.27 or newer, `make` and bash. It builds the CLI and runs `twinwright demo` in a temporary directory: no network, no API key, no container runtime. The demo has 12 steps over one incident: compile the four-service world, run it under injected faults, inspect, trace, evaluate, replay, fork and compare, crash a worker and let another take over, fail the same incident on purpose and explain the failure, block a prompt injection and an over-privileged request, then block a regressed agent at the CI gate. Each step reads its evidence back out of what it just ran, and the first step that cannot produce its evidence stops the walkthrough with a non-zero exit. Without `make`: `go run ./cmd/twinwright demo --dir ./demo-out`. `make demo` writes to a temporary directory that it removes when the demo succeeds and keeps, with its path printed, when it fails.

```text
=== 8/12 crash a worker and watch another take over
evidence: 3 crashes, 3 takeovers at fence 1 -> 2, 3 stale commit refused by fencing, replay verified on all 3

=== 12/12 block a regressed agent at the CI gate
evidence: exit 1: 19 regressed cases in 21 compared, including billing-duplicate-charge
```

That is real output, two of the twelve steps. [docs/guide/quick-start.md](docs/guide/quick-start.md) has more of it and the commands to run the incident by hand. The `end-to-end` job of the CI workflow runs `make demo` on every pull request and every push to `main`.

### `make demo-stack`: the whole platform, Docker required

```bash
make demo-stack
```

Needs Docker with Compose, plus bash, curl, jq and Go. It starts the Compose PostgreSQL, OpenTelemetry collector, Jaeger and Prometheus, recreates two databases of its own (`twinwright_demo_company` and `twinwright_demo_billing`, never `twinwright`), queues eight runs, and runs three workers on the host that export their traces to the collector. Worker A is killed with `SIGKILL` while it holds a run, and worker B takes the run over after the 5 second lease lapses. The script exits non-zero unless six checks hold: all eight runs completed; the ownership log shows B taking over A's lease; Prometheus counts the takeover; Prometheus scraped both workers; every run's trace is queryable in Jaeger by its trace ID; and the benchmark gate passes against the approved baseline.

In three local runs on WSL2 with Docker all six checks held. The `demo-stack` job of the Integration workflow runs it. It passed on its first run on GitHub Actions on 2026-10-28 with all six checks: 8 of 8 runs completed, the ownership log showed demo-worker-b taking over demo-worker-a's lease, Prometheus counted the takeover, both workers were scraped, 8 of 8 traces were queryable in Jaeger, and the benchmark gate passed. It passed again on every Integration run after that. Those runs predate the port checks, `TW_DEMO_PG_PORT` and the kept failure files described below. The `pull_request` run of the Integration workflow on 2026-10-29 ran the script with all of them and the job passed, so all six checks held on a runner; the steps that print and upload the failure files run only when the job fails, and that run did not run them. The stack is left running; `make down` stops it and deletes the Compose volumes. While the demo runs, its two metrics ports (9095 and 9096) listen on all interfaces with no authentication.

Ports it needs free: PostgreSQL's host port (5432 by default), 4318, 13133, 16686, 9090, 9095 and 9096. It refuses to start when 9095 or 9096 already answers, and before it relies on Prometheus it checks that each port serves its own worker's `twinwright_worker_claims_total` series. If something else holds 5432, set `TW_DEMO_PG_PORT` to another host port: `docker-compose.yml` publishes PostgreSQL on that port and the script connects to it, so one variable changes both (`TW_DEMO_PG_PORT=55432 make demo-stack`). The script's temporary directory (the built CLI, the manifests, the worker logs) is removed when all six checks hold and kept, with its path printed, when one does not; `TW_DEMO_LOGS=<dir>` copies the logs out on exit.

## The regression gate in CI

`twinwright bench` runs a suite of scenarios with deterministic judges and writes a JSON report. `twinwright gate` compares a candidate report with an approved baseline and exits 0 for no regressions, 1 for regressions, and 2 when it cannot decide, so CI can block on the first and page someone about the second.

```bash
go run ./cmd/twinwright bench --suite standard --agent scripted --examples examples --out candidate.json
go run ./cmd/twinwright gate \
  --baseline examples/bench/baselines/standard-scripted.json \
  --candidate candidate.json --markdown "$GITHUB_STEP_SUMMARY"
```

A gate that has only ever passed has not been shown to catch anything, so the repository shows it failing. `--model fixture-regressed-v1` selects a scripted agent that investigates and then claims success without making the first write. The `regression-gate` job of the CI workflow requires the gate to exit exactly 1 on it, and `TestRegressedBenchFailsTheGate` holds the same pair in `go test`. `run --fail-on-eval` is the single-run version: it exits non-zero after printing the result when the evaluation fails. See [docs/guide/bench.md](docs/guide/bench.md) and ADR 0030.

CI gates the scripted standard suite only. No live-model report is gated. The final-state digest the design first called for was cut: the gate fails on a baseline case that went missing, passed and now fails, a negative control that stopped being caught, more committed mutations, more agent refunds, more successful violations, or a new unsafe retry, and by default only warns when the tool-call sequence changed.

## Run your own agent

`--agent exec` starts a program of your choice once per turn, writes one JSON request to its stdin (the task, the history and the operations the run exposes) and reads one JSON response from its stdout: tool calls to propose, or a final answer. The agent proposes and the dispatcher still authorizes and executes, so a run's policy does not depend on the agent behaving.

```bash
go run ./cmd/twinwright build examples/billing/openapi.yaml
go run ./cmd/twinwright run duplicate-charge \
  --agent exec --model billing-agent@1 \
  --agent-command 'python3 examples/agents/python/billing_agent.py' \
  --fail-on-eval
```

The protocol is `twinwright.agent.v1`, documented in [docs/protocol/agent-v1.md](docs/protocol/agent-v1.md) with JSON Schema files for the request and response. `examples/agents/python/billing_agent.py` is a deterministic, standard-library reference agent that solves `duplicate-charge`; it is a protocol example, not a model. The child process gets only `PATH`, `HOME` and the variables named with `--agent-env`, never the database address or a provider key. The command line is not stored with the run, so `resume` and `worker` need `--agent-command` again.

**An exec agent runs with your privileges.** Twinwright does not stop it reading files or opening connections. `--agent-container IMAGE` runs it under `docker run --rm -i --read-only --cap-drop ALL --security-opt no-new-privileges --user 65534:65534` with `--network none` by default and memory, CPU and process caps. What that does not do: the Docker daemon is trusted, there is no custom seccomp profile, a network you allow is unrestricted, on macOS and Windows the boundary is the Docker Desktop VM, and a container is left running if Twinwright itself is killed mid-turn (a turn that times out, is cancelled or floods its output has its container removed). See ADR 0033 and section 7 of [docs/security-review.md](docs/security-review.md).

## Real models and cassettes

`--agent` selects `scripted` (in-process fixtures), `openai`, `openai-compatible`, `anthropic` or `exec`. The world, dispatcher and evaluator never see vendor details. A provider call has a per-attempt deadline (`--provider-timeout`), an optional output ceiling (`--max-output-tokens`), and a retry policy: a timeout, a truncated completion, a 429, a 500, 502, 503 or 504 (an Anthropic 529 too) or a dropped connection is retried once and every attempt is recorded as a `provider.interrupted` event. See [docs/guide/providers.md](docs/guide/providers.md).

Replay from the ledger reproduces a run's bookkeeping but never runs the adapter. A cassette puts the adapter in the loop: `run --record-cassette PATH` writes every HTTP exchange of the provider to a JSON-lines file, and `replay <run> --cassette PATH` re-executes the run through the run's own adapter, answered from that file. It needs neither an API key nor a base URL, and the cassette transport never dials. Request headers are never recorded and the configured key is masked in both bodies. **A cassette holds the model's prompts and answers, so do not commit one.**

`scripts/live-model.sh` (`make live-model`) runs `duplicate-charge` against a local Ollama model, records it, and replays it from the ledger and from the cassette. The `real-model` job of the Integration workflow runs it on `qwen2.5:1.5b` with the pinned `ollama/ollama:0.40.11` image and then replays from the cassette with the server stopped. The job gates on recording and replay, not on whether the model solves the scenario. It passed on its first run on 2026-10-28 and on every Integration run since. The one run recorded in [docs/live-model-run.md](docs/live-model-run.md) did not solve it: the 1.5B model made one tool call, then answered in text, and the evaluation failed. Both replays verified. That is one sample on one machine and says nothing about a hosted provider.

**Hosted providers are unverified.** The OpenAI, OpenAI-compatible and Anthropic adapters run against deterministic local HTTP servers. `TestLiveOpenAIToolCall` and `TestLiveAnthropicToolCall` make one real call each and are opt-in; they have never run in this repository's CI.

## The fleet

For more than one process, point Twinwright at PostgreSQL and run workers. Each claims queued runs under a fenced lease.

```bash
docker compose up -d postgres
export TW_DSN='postgres://twinwright:twinwright@127.0.0.1:5432/twinwright?sslmode=disable'
go run ./cmd/twinwright build examples/billing/openapi.yaml \
  --bindings examples/billing/bindings.yaml --out twinwright.manifest.json
go run ./cmd/twinwright run duplicate-charge --db "$TW_DSN" --agent scripted --enqueue --steps 10
go run ./cmd/twinwright worker --db "$TW_DSN" --lease-ttl 30s --metrics-addr 127.0.0.1:9095
go run ./cmd/twinwright queue --db "$TW_DSN"
```

- **Leases and fencing.** Every acquisition raises a fence. Every durable write proves, inside the same transaction, that this worker still owns the run, so a worker that stalled past its lease cannot commit after another took over.
- **Takeover.** A run is claimable when it is runnable, or leased with an expired lease, which is what a killed worker leaves behind. No janitor is needed.
- **At-least-once delivery.** A crashed worker's run is attempted again. A world mutation commits at most once per run and call ID, because the effect, its ledger events, its saved result and its transcript entry commit in one transaction. Model calls are at-least-once: a crash before a response is recorded repeats the provider call and its cost. A retry under a new call ID is a new action and is not deduplicated. Provider spend, logs and metrics are not fenced. Exactly-once is not claimed.
- **Crash boundaries.** A run can die at seven named points. `TestCrashMatrixSQLite` crashes every occurrence of every point across three scenarios (120 cases, zero violations). `TestRealProcessDeathIsRecovered` (unix) kills real worker processes with `SIGKILL` at four of those points. The PostgreSQL versions run when `TWINWRIGHT_TEST_POSTGRES_DSN` is set, in the Integration workflow.
- **Delivery bound.** A run that keeps killing its worker is failed after `MaxAttempts` (default 3) deliveries instead of being taken over forever. A run retried after that can complete but cannot be verified by replay.
- **Local runs hold a lease too.** `run` and `resume` without `--enqueue` take a 15 second lease, so they cannot race a fleet worker.

[docs/distributed.md](docs/distributed.md) has the topology, the takeover sequence, the lease lifecycle and a table of what is durable at each crash point. [docs/guide/distributed-runtime.md](docs/guide/distributed-runtime.md) has the semantics, storage and upgrade notes.

## Observability

Traces are projected from the durable ledger, so they can be produced long after a run finished. `trace --format otlp --otlp-endpoint URL` posts one OTLP/HTTP JSON document to a collector and reports the status code and trace ID; a rejection is an error, and so is a 200 that reports rejected spans. `worker --otlp-endpoint` exports the trace of each run it finishes, in the background and best effort, one export at a time, without changing the claim's outcome. Every span carries `twinwright.event.start_seq` and `twinwright.event.end_seq`, so a span can be traced back to ledger events.

`docker compose up -d` starts PostgreSQL, the collector, Jaeger and Prometheus, all published on loopback. A worker serves Prometheus metrics with `--metrics-addr`. **The database-derived gauges are served by every worker, so with several workers on one database scrape one of them or aggregate with `max`, never `sum`.** Making them fleet-wide was cut.

The `observability` job of the Integration workflow checks two claims against the real services: `TestSpansAreQueryableInJaeger` exports a run through the collector and reads it back from Jaeger by trace ID, and `TestPrometheusScrapesAWorker` has Prometheus scrape a worker and answer a query. The job passed on its first run on 2026-10-28 and on every Integration run since. **None of the four services has TLS or authentication, and the metrics endpoint is unauthenticated.** See [docs/guide/observability.md](docs/guide/observability.md) and ADR 0032.

## Benchmarks

`twinwright perf --out perf.json` measures four workloads on the scripted `duplicate-charge` scenario: fleet throughput at 1, 2, 4 and 8 in-process workers, crash recovery time against the lease TTL, replay verification, and the crash matrix. Each report records the backend, configuration, build, Go version, operating system, CPU count and seed, and exits 1 if a recovery duplicated a mutation, a replay did not verify or the crash matrix reported a violation. [docs/performance.md](docs/performance.md) holds the methodology, the results copied from two committed reports in `docs/benchmarks/`, and what was not measured.

One result is unfavourable: **SQLite throughput did not scale with workers on the measured machine** (two workers were slower than one, eight faster than four), and its cause was not investigated. PostgreSQL throughput rose with workers on a GitHub runner. The two reports come from different machines and operating system layers, so they are not a comparison of the backends, and each is one machine with three trials. See ADR 0034.

## Limitations

Stated plainly, because a harness that overstates itself is worse than one that admits a gap. The long list is in [docs/guide/limitations.md](docs/guide/limitations.md).

- **No hosted service.** Self-hosted only: no accounts, tenants, web UI or control-plane API. Kubernetes, brokers, sharding and multi-region are out of scope.
- **Hosted providers are unverified.** The only live-model runs are of one small local model, and the recorded one failed the scenario.
- **Delivery is at-least-once.** World mutations are made safe, not prevented from being retried. Provider spend, logs and metrics are not fenced.
- **Crash evidence is bounded.** Seven boundaries, three scenarios; the real-process test kills one process at a time and does not show multi-process contention on one SQLite file.
- **A run retried after the delivery limit cannot be replayed.** Replay of runs that ended on a non-retried provider error was cut.
- **`make demo-stack` has passed in CI,** on each Integration run it was part of, but the stack it starts has no TLS or authentication.
- **Benchmarks are one machine each.** Three micro-benchmarks have no figure, and nothing was optimised on these numbers.
- **Shadow mode** reads recorded files only and has never been tested against a live external system. **Container sidecars** are experimental.
- **Read-only opens of a SQLite database in WAL mode** create `-wal` and `-shm` files, so `replay`, `trace`, `evaluate`, `inspect`, `queue` and `doctor` need a writable directory.
- `SECURITY.md` is the policy and how to report a vulnerability. [docs/security-review.md](docs/security-review.md) is the review pass over each internal boundary.

## Development

```bash
git clone https://github.com/omorsi45/twinwright
cd twinwright
go test ./...        # hermetic: no network, no API key, no container runtime
./scripts/e2e.sh     # the whole documented pipeline, with scripted fixtures
make check           # gofmt, vet and build, the way CI runs them
make test-race       # the worker runtime is concurrent
```

`go test ./...` stays hermetic. External dependencies are opt-in by environment variable: `TWINWRIGHT_TEST_POSTGRES_DSN`, `TWINWRIGHT_TEST_DOCKER=1`, `TWINWRIGHT_TEST_OTEL=1`, `TWINWRIGHT_TEST_LIVE_OPENAI=1` and `TWINWRIGHT_TEST_LIVE_ANTHROPIC=1`. `make verify-full` adds the PostgreSQL tests (start the stack with `make up`). `make help` lists the rest, and [CONTRIBUTING.md](CONTRIBUTING.md) describes what a change is expected to prove.

| Read | For |
| --- | --- |
| [docs/claims.md](docs/claims.md) | every claim, with the test or CI job that proves it |
| [docs/architecture.md](docs/architecture.md) | components, the path of one tool call, trust boundaries, the ADR index |
| [docs/guide/](docs/guide/README.md) | the detail that used to be in this file, and where each section went |
| [docs/performance.md](docs/performance.md) | benchmark methodology and results |
| [CHANGELOG.md](CHANGELOG.md) | what changed, newest first |
| [docs/guide/repository.md](docs/guide/repository.md) and [docs/guide/development.md](docs/guide/development.md) | repository layout and the development workflow |

## License

Apache License 2.0. See [LICENSE](LICENSE).
