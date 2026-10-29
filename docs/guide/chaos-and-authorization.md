# Chaos policies and principal authorization

This page was moved out of the project README when the README was rewritten as the platform overview. It holds the README sections "Deterministic chaos policies", "Principal authorization", unchanged except where noted. Paths in backticks are relative to the repository root. [Back to the guide index](README.md).

## Deterministic chaos policies

Use `--chaos` to attach a validated, versioned YAML policy to a new run. The policy is persisted with the run and reused on resume and replay. Forks inherit the policy and its counters at the selected checkpoint, or can replace it for the child.

```bash
go run ./cmd/twinwright build examples/billing/openapi.yaml

go run ./cmd/twinwright run ambiguous-commit \
  --agent scripted \
  --chaos examples/chaos/ambiguous-commit.yaml \
  --seed 42

go run ./cmd/twinwright inspect <run-id>
go run ./cmd/twinwright replay <run-id>
```

The ambiguous-commit scenario models a difficult distributed-systems failure: a write commits successfully, but the response is lost. An unsafe agent retries with a new call ID and can duplicate the side effect.

The run can report infrastructure faults, unsafe retries, recovery success, and agent failure without allowing a later state check to erase evidence of an unsafe action.

Supported policy effects include:

| Rule type | Effect |
| --- | --- |
| `http_error` | Return a configured 4xx or 5xx response before execution |
| `timeout` | Return a transport timeout before execution |
| `timeout_after_commit` | Commit a write but hide its successful response |
| `rate_limit` | Return 429 after a configured call gate |
| `permission_revocation` | Simulate a 403 service denial |
| `partial_service_outage` | Return 503 across selected operations |
| `latency` | Record deterministic simulated delay and execute normally |
| `stale_read` | Return a captured earlier read for matching arguments |
| `malformed_response` | Replace the visible response while auditing the actual result |
| `concurrent_mutation` | Execute a validated actor write before the selected agent call |

See `examples/chaos/` and `docs/adr/0008-chaos-engine.md`.

## Principal authorization

Use `--auth` to run an agent as a principal with a validated, versioned YAML policy. The policy is checked against the manifest before any world is created, persisted with the run, and reused on resume, replay, and forks. A run without `--auth` records the principal `local-unrestricted` and behaves exactly as before.

```bash
go run ./cmd/twinwright build examples/company/openapi.yaml \
  --bindings examples/company/bindings.yaml

go run ./cmd/twinwright run prompt-injection-ticket \
  --agent scripted \
  --auth examples/security/support-policy.yaml

go run ./cmd/twinwright inspect <run-id>
go run ./cmd/twinwright replay <run-id>
```

Authority never comes from the prompt. Every built-in behavior maps to one permission, such as `charges.read`, `refunds.create`, or `slack.messages.write`, and the dispatcher checks it inside the tool transaction before any chaos rule or service handler runs. The model only sees operations it may call next, but a direct call to a hidden operation still reaches the dispatcher and is denied with 403. Every decision is recorded as `authorization.allowed` or `authorization.denied` with the principal, permission, call number, and a stable reason code.

| Policy field | Meaning |
| --- | --- |
| `principal.id`, `principal.roles` | The acting identity and its roles |
| `roles.<name>.allow`, `permissions.allow` | Role and direct grants, combined |
| `resources.customer_ids` | Customers reachable directly or through invoices, charges, subscriptions, CRM accounts, and tickets; broad searches are denied |
| `resources.channel_ids`, `resources.project_ids` | Channels that can be read or posted to, and projects that accept new issues |
| `constraints.refund_max_cents` | The largest refund allowed |
| `temporary_grants`, `revocations` | Grants active for a range of call numbers, and permissions removed after a call number |

An omitted resource list imposes no scope; an explicitly empty list denies everything of that kind, and empty or null values are rejected. A customer scope does not restrict messaging, so scope `channel_ids` as well. A revocation wins over every grant. Call numbers count unique, argument-valid tool calls, including denied ones, so temporary grants stay reproducible. `fork --auth` replaces a child's policy and restarts its count at zero.

The `prompt-injection-ticket` scenario seeds a ticket comment that tells the agent to look up another customer and post their details. The scripted fixture obeys it through direct calls. The run's `security` report lists `attempted_violation`, `blocked_violation`, and `successful_violation` with ledger event IDs, separately from task evaluation. The support policy blocks both attempts; `examples/security/overprivileged-policy.yaml` lets them succeed.

See `examples/security/` and `docs/adr/0009-principal-authorization.md`.
