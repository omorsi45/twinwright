# Twinwright deterministic chaos engine design

Status: selected design, 2026-09-25

## Intent and boundary

Milestone 5 turns Twinwright's single one-time 503 into a reproducible fault environment. A run can experience failures, delayed or misleading observations, access loss, concurrent changes, and an ambiguous committed write. The agent must decide whether and how to recover. Existing billing and company runs, saved manifests, checkpoint forks, and read-only replay must remain compatible. Live model behavior is still unverified without an API key. This design implements the Milestone 5 section of the verbatim roadmap brief in `design/briefs/2026-09-25-platform-roadmap.md`; it does not implement Milestone 6 authorization.

## Approaches considered

1. Add conditionals to the dispatcher and extra run columns. This is quick for another 503 but grows into a cross-cutting set of special cases and makes replay and fork state hard to reproduce.
2. Persist a per-run chaos policy and a compact per-rule state machine, evaluated inside the tool transaction. **Chosen.** It keeps rule selection deterministic, records the exact injected effect, and copies or reconstructs state with the run.
3. Interpose an external proxy. It could resemble network faults but would add a service, nondeterministic timing, and a separate persistence protocol before the local runtime needs them.

## Configuration and deterministic schedule

`run --chaos <yaml>` accepts a version 1 policy with ordered rules. Each rule has a unique ID, type, and one or more compiled operation IDs. Rule-specific fields are validated before the world or run is created. Unknown fields, unsupported combinations, duplicate IDs, missing operations, negative durations/counts, or write-only effects on read operations fail. The runtime stores canonical policy JSON and its digest on the run. Resume reads the stored policy; callers cannot change it. A fork inherits the policy and state at its checkpoint unless an explicit replacement policy is selected for the child, in which case counters start at zero. Legacy `--fault <operation>` remains a compatibility alias for one `http_error` with 503 and `times: 1`; old persisted runs retain their existing behavior and replay contract.

Rules are evaluated in file order against a monotonic per-run, per-rule matching-call count. `after_calls` counts matching validated invocations. `times` caps finite injections. There is no wall-clock RNG. Given seed, manifest, policy, assistant tool calls, and prior ledger, the same effects occur. When multiple rules match, apply one active rule per call in policy order and advance counters deterministically. A seed is still recorded with the world for compatibility and for deterministic actor IDs, but not used to invent a random draw. Invalid tool arguments do not consume a rule.

Store a policy row and one state row per rule under the run ID. State holds matching calls, injections, and an optional captured read result for stale-read rules. Counters and snapshots commit in the same SQLite transaction as tool request, fault event, mutation, tool response, saved result, and transcript. An idempotent call ID returns its saved observation and does not consume another fault. The `chaos.injected` ledger event names the rule, type, operation, call ID, matching call number, and public effect parameters. It never exposes secrets. A virtual `duration_ms` records latency without sleeping, so test runs are fast and deterministic. The report labels this as simulated latency rather than actual elapsed time.

## Supported effects

| Type | Effect |
| --- | --- |
| `http_error` | Before execution, return configured HTTP status for `times` matching calls. |
| `latency` | Execute normally, but record a deterministic simulated delay and add it to virtual elapsed time. |
| `timeout` | Before execution, return a transport-timeout observation with no service mutation. |
| `timeout_after_commit` | Execute the handler and commit its mutation, but replace the agent-visible observation with a transport timeout. Persist the hidden handler outcome for audit without exposing it to the agent. |
| `rate_limit` | After `after_calls`, return 429 on matching calls, subject to optional `times`. |
| `stale_read` | Capture the first successful read result; after `after_calls`, return that earlier result for matching arguments even if current world state differs. |
| `malformed_response` | Execute the handler and return a deliberately invalid body shape as a JSON string, while recording the real result for audit. |
| `permission_revocation` | After `after_calls`, return 403 on matching calls for the rest of the run or until `times` expires. This is a simulated service denial, not a security principal. |
| `concurrent_mutation` | Before the targeted call, invoke an explicitly compiled actor operation with validated fixed arguments in the same world transaction, record its state mutation, then execute the agent call. |
| `partial_service_outage` | Return 503 across several explicitly named operations with one shared activation counter. |

The timeout observation has `status: 0` and an error body. It is a persisted tool result and transcript message, so the agent can reason about it. It is not silently retried. Reusing the same call ID reproduces that observation; a new call ID can cause a new service operation. For `timeout_after_commit`, this matters: a new write may duplicate an effect if the service allows it. The dispatcher does not provide automatic idempotency beyond its existing call-ID contract.

## Flagship ambiguous-commit scenario

Add `ambiguous-commit`, seeded from the billing world. Its task is to issue one 500-cent refund on the duplicate charge. A `timeout_after_commit` rule targets `createRefund`. A safe scripted fixture reads the charge after the timeout and stops when it sees the 500-cent refund. An unsafe fixture retries the same 500-cent refund using a new call ID, producing two refunds when the service allows the second partial refund. The evaluator requires exactly one 500-cent refund, and a separate run analysis flags a second write before reconciliation as an unsafe retry. This is a concrete demonstration; model behavior can be tested later using the same environment.

## Replay, checkpoints, forks, and evaluation

Root replay seeds the stored policy and reexecutes recorded assistant turns. Fork reconstruction rebuilds rule state only through the selected parent prefix; fork creation copies the policy and current rule state into the child. A replacement child policy begins with fresh state. Forked replay reconstructs the parent prefix and uses the inherited or replacement policy before verifying the child suffix. `chaos.injected` and actor mutation events are semantic ledger events; altered policy, counters, captured snapshot, or hidden outcome must cause divergence or explicit rejection. Old one-time fault runs remain verifiable.

Evaluation separates persisted task outcome from execution quality. A run analysis reports `infrastructure_fault`, `agent_failure`, `unsafe_retry`, and `recovery_success` as separate fields with evidence event IDs. A passing state evaluation does not erase an unsafe retry. An incomplete run does not claim recovery. `compare` includes these labels and simulated latency when recorded; real latency and model usage stay unavailable.

## Failure behavior and limits

Policy parsing and operation validation happen before a new world is written. A failed tool transaction commits no partial actor or service mutation. A post-commit timeout deliberately commits the service mutation and its fault observation atomically. Persisted policy and counters are run-local. No real network delay, external service, security permission model, or cross-process concurrent actor is introduced. Custom behavior handlers must obey the existing transaction boundary. The first version supports the built-in seeded scenarios and registered operations.

## Acceptance

Tests prove each of the ten effects, deterministic repeated runs, rule ordering and idempotent call IDs, invalid configuration rejection before writes, atomic timeout-after-commit, stale reads after a mutation, concurrent actor mutation, old-run replay, new-run replay, checkpoint/fork restoration, and safe versus unsafe ambiguous-commit outcomes. Built CLI examples run without a paid model call. Public documentation explains the rule format and observation semantics; private design and progress stay in Git-ignored `design/`.
