# Twinwright multi-service company world design

Date: 2026-09-25
Status: implementation design

## Purpose

Milestone 2 extends the current deterministic billing world with stateful CRM, ticketing, and messaging services. It must support a cross-service customer-support scenario and variants while preserving the existing `duplicate-charge` flow, manifest digest validation, atomic local tool commits, idempotent call IDs, and read-only replay.

## Scope and capability boundary

This milestone adds a second, explicitly supported company OpenAPI example. The compiler still accepts only known operation IDs, exact routes, exact primitive argument schemas, and explicit behavior bindings. Generic service descriptions and a registration interface belong to Milestone 3. No network services or actual Jira/Slack accounts are used.

## Service state

All rows carry `world_id` and are seeded inside the existing world transaction. Add `subscriptions` for billing; `crm_accounts`, `crm_contacts`, `crm_notes` for CRM; `ticket_projects`, `ticket_issues`, `ticket_comments` for ticketing; `message_workspaces`, `message_channels`, `message_members`, `message_messages` for messaging. Foreign IDs connect CRM accounts to billing customers and issues/messages to the relevant account and support channel. IDs and virtual timestamps are deterministic from seed, world definition, and stable call IDs. World isolation continues to be enforced by `world_id` in reads and writes.

## Operations

The company manifest includes the existing five billing operations plus a subscription read; CRM account lookup/search, note creation, and status update; ticket issue create/get/search, comment, and transition; and channel list/read/message post. Each operation has an exact route and binding. Search reads current rows. Mutations have business validation and return current state on subsequent reads. Read operations never synthesize answers from static JSON. Mutating handlers return a typed mutation payload for the existing transaction ledger.

The dispatcher keeps one transaction around argument validation, fault injection, service handler, mutation event, saved tool result, and transcript advance. It routes only by the explicit behavior prefix, not by inferred OpenAPI semantics. Unknown bindings fail at compile/manifest validation. The current one-shot 503 remains available for any compiled operation. A saved call ID returns its committed result when retried with identical arguments and rejects changed arguments.

## Scenarios and evaluation

Keep `duplicate-charge` unchanged. Add `company-incident`, `company-routine`, and `company-no-duplicate`. The incident fixture includes duplicate billing and evidence of a software issue: the agent should refund one duplicate, add a CRM resolution note, create a ticket, and notify the designated support channel. The routine fixture has a duplicate but no software-incident signal: refund and CRM note are required; no ticket or channel post. The no-duplicate fixture has one legitimate charge: no refund, a CRM note explaining the finding, and no ticket or post.

The scripted provider exists only as a deterministic fixture; it reads tool results and follows each variant. Live models receive the same scenario task and operations. The evaluator reads database state, checking the correct customer, charge/refund amount/count, CRM note, ticket/post expectations, and absence of unrelated-world or unrelated-account mutation. More than one read trajectory can pass if the final state satisfies the contract. Fault injection and pause/resume remain testable.

## Replay and compatibility

Verification replay seeds the target with the same scenario, manifest digest, and seed, then feeds recorded assistant turns through the normal runner. It compares semantic ledger events, saved tool results, transcript, and every table relevant to that scenario. Old `duplicate-charge` runs compare only their original billing tables so they remain replayable after new tables are added. New scenarios compare all service state. The source DB opens read-only. The replay cannot claim historical-runtime compatibility.

## Implementation boundaries

Keep service handlers in separate `internal/crm`, `internal/ticketing`, and `internal/messaging` packages, using the caller's SQL transaction. Add scenario-aware seeding to `store` while retaining `Seed` as the original billing entry point. Extend the known compiler contracts without broad schema generalization. Extend CLI wiring for the new scenarios and keep README current. Record the scoped multi-service design in a public ADR, while this working spec stays in the ignored `design/` folder.

## Failure semantics

Unknown resources return 404; invalid arguments return 400; conflicts such as illegal issue transitions or repeated business mutations return 409. SQL failures roll back state, mutation event, result, and transcript together. A one-time 503 commits only the fault response and ledger event, not a service mutation. Unsupported OpenAPI features and bindings fail build or manifest validation. A failed live provider turn remains inspectable and does not imply evaluator success.