# Safety, scope and current limitations

This page was moved out of the project README when the README was rewritten as the Twinwright Cloud overview. It holds the README sections "Safety and scope", "Current limitations", unchanged except where noted. The live-provider and performance bullets were corrected, and one link was adjusted. Paths in backticks are relative to the repository root. [Back to the guide index](README.md).

## Safety and scope

Twinwright's included worlds are fictional local simulations.

The project does not require production credentials for its deterministic examples and does not connect to real billing, CRM, ticketing, or messaging systems by default.

Principals and permissions are local, deterministic simulations with no external identity provider. The `permission_revocation` chaos effect is a simulated service denial, separate from principal authorization.

Keep provider credentials outside the repository.

`SECURITY.md` is the policy and how to report a vulnerability.
[docs/security-review.md](../security-review.md) is the review pass over each
internal boundary - the authorization boundary and its deny reasons, prompt
injection through world content, secret redaction, the container surface, the
shadow connector, SQL construction - naming the test that holds every claim and the
places where no test holds one. A test fails the build if the document cites a file
or test that no longer exists.

## Current limitations

Stated plainly, because a testing harness that overstates itself is worse than
one that admits a gap.

- **Hosted-provider runs are unverified in this repository.** The OpenAI,
  OpenAI-compatible and Anthropic adapters are exercised against deterministic
  local HTTP servers, and the chat completions adapter has also been run against
  a local Ollama model: one run is recorded in `docs/live-model-run.md` (the model
  failed the scenario, and the recording replayed with the server stopped), and the
  Integration workflow's `real-model` job runs the same thing. `TestLiveOpenAIToolCall`
  and `TestLiveAnthropicToolCall` would call the paid APIs, are opt-in, and have never
  run in this repository's CI, so no claim is made about hosted-provider behaviour
  beyond adapter conformance. Truncation detection
  depends on the provider sending its stop reason: a surface that omits it, or a
  gateway that rewrites it to a success value, gives nothing to check and the
  response is treated as complete.
- **Container execution is experimental, and its live-daemon evidence comes from
  CI rather than from a development host.** The lifecycle is hardened: readiness
  gating, bounded startup, cleanup with captured logs on failure, graceful stop,
  explicit network, and validated resource limits. Unit tests use an injected
  runner; the real-daemon tests are gated on `TWINWRIGHT_TEST_DOCKER=1` and run
  in the Integration workflow. No world depends on a sidecar, and container
  identity is not carried in traces, because no sidecar is associated with a run.
- **Shadow mode reads recorded files only.** Three connectors decode native
  JSONL, sanitized audit logs and recorded HTTP interactions, all as pure
  functions of bytes. There is deliberately no webhook or event-stream
  transport: a listening socket taking unauthenticated input is a different
  threat model, not another registry entry. Write mode is rejected by design, and
  no live external integration has been tested, which the CLI reports as
  `live_external: false`. The comparison pairs a diverging action only when both
  sides name the same resource, so a call carrying no identifying argument stays an
  unpaired residual rather than being guessed at, and elapsed time is reported as
  not compared rather than as zero.
- **The distributed runtime is a multi-worker fleet over one database.** It is not
  multi-region, has no broker, and does not shard. A worker is a process that
  needs a DSN; how it is scheduled is an operational choice.
- **Delivery is at-least-once.** Exactly-once is never claimed. Duplicate
  deliveries are made safe by atomic commit, call-ID idempotency and fencing, not
  prevented. That covers world mutations only: a crash before a model response is
  recorded repeats the provider call and its cost, and provider spend, logs and
  metrics are not fenced.
- **A run retried after the delivery limit is not replay-verifiable.** Re-enqueueing
  a run that failed at the delivery limit starts a fresh delivery budget and the
  run can complete, but its ledger records the `delivery_limit` error and an
  unanswered model request, which `replay` refuses. Accepting such a ledger is
  deferred to a later milestone; ADR 0029 lists it under remaining limits.
- **Crash recovery claims are bounded.** The crash matrix
  covers seven named boundaries across three scenarios, and the real-process test
  kills one process at a time at four points, so it does not show concurrent
  multi-process contention on one SQLite file. `after-completion` is simulated by
  skipping `FinishRun`. The PostgreSQL matrix and the PostgreSQL real-process test run
  only when a DSN is set. The `postgres` job of the Integration workflow sets one and
  runs the whole suite against PostgreSQL 16 and 17; it passed on every Integration
  run of this branch on 2026-10-28, so those tests have run against a server there.
  `fork`, `counterfactual`, the non-distributed `bench`
  modes and the shadow simulation execute their own new runs without a lease.
- **The metrics endpoint is unauthenticated** and off by default. Bind it to
  loopback.
- **Published performance numbers cover two machines, and three micro-benchmarks
  have no figure at all.** `docs/performance.md` reports what a Windows laptop
  measured stably and names the three benchmarks whose wall clock moved while
  their allocation counts stayed identical, which means the measurement described
  the host rather than the code. PostgreSQL 17 figures from one GitHub runner are in the Platform benchmarks section of
  `docs/performance.md`; they come from a different machine and operating system layer, so they
  are not a comparison with the SQLite figures. Nothing has been optimised on the strength of
  these numbers; they are a baseline.
- **Read-only opens of a SQLite database in WAL mode create `-wal` and `-shm`
  sidecars**, so `replay`, `trace` and `evaluate` need a writable directory even
  though they never write to the database itself.
