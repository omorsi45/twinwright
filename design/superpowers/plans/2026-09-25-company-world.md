# Multi-Service Company World Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add deterministic, stateful billing, CRM, ticketing, and messaging behavior plus three cross-service scenario variants without weakening the current billing flow or replay.

**Architecture:** Keep one Go runtime and SQLite transaction boundary. Extend the explicit known-operation compiler, seed scenario-specific world rows, route behavior to separate service packages inside the existing dispatcher transaction, and evaluate final state from database rows. Feed recorded model turns back through the same runner for new-scenario replay.

**Tech Stack:** Go 1.27.1, SQLite through modernc.org/sqlite, existing YAML parser, existing CLI and tests.

**Spec:** `C:\Users\Omar Morsi\Desktop\Projects\twinwright\design\superpowers\specs\2026-09-25-company-world-design.md`

## Global constraints

- Keep `duplicate-charge` output, `Seed`, five billing operation contracts, and old replay behavior compatible.
- Every query filters `world_id`; every mutation, ledger event, saved result, and transcript advance share one SQL transaction.
- Function tool IDs are globally unique inside a manifest and use exact explicit behavior bindings.
- A repeated `(run_id, call_id)` with identical arguments returns the saved result; changed arguments fail.
- No paid model call in tests. Test the scripted fixture and stateful handlers locally.
- Unsupported OpenAPI shapes or bindings fail explicitly.
- Save working plans and specs under ignored `design/`; publish only ADRs and user-facing docs.

## Contract map

| Operation | Method and path | Binding | Arguments |
| --- | --- | --- | --- |
| getSubscription | GET `/subscriptions/{id}` | billing.getSubscription | id:string |
| crmGetAccount | GET `/crm/accounts/{id}` | crm.getAccount | id:string |
| crmSearchAccounts | POST `/crm/accounts/search` | crm.searchAccounts | query:string |
| crmAddAccountNote | POST `/crm/notes` | crm.addAccountNote | account_id:string, body:string |
| crmUpdateAccountStatus | POST `/crm/accounts/status` | crm.updateAccountStatus | account_id:string, status:string |
| ticketCreateIssue | POST `/tickets/issues` | ticket.createIssue | project_id:string, account_id:string, title:string, priority:string |
| ticketGetIssue | GET `/tickets/issues/{id}` | ticket.getIssue | id:string |
| ticketSearchIssues | POST `/tickets/issues/search` | ticket.searchIssues | query:string |
| ticketAddComment | POST `/tickets/comments` | ticket.addComment | issue_id:string, body:string |
| ticketTransitionIssue | POST `/tickets/issues/transition` | ticket.transitionIssue | issue_id:string, status:string |
| messageListChannels | GET `/messages/workspaces/{id}/channels` | messaging.listChannels | id:string |
| messageReadChannel | GET `/messages/channels/{id}` | messaging.readChannel | id:string |
| messagePostMessage | POST `/messages/messages` | messaging.postMessage | channel_id:string, body:string |

The existing five billing operations retain their contracts. Search operations are POST requests with query bodies because the current compiler supports only path parameters and JSON request bodies. They do not mutate state.

## Review focus

- A missing or invalid world/account/issue/channel ID must return a deterministic 404 or 400 without mutation.
- Reusing a call ID with changed arguments must fail before invoking any handler.
- A one-time 503 on a mutation must create no service row; a new call can succeed once.
- Different seeded worlds must never share CRM, ticket, or message mutations.
- Replay of old billing runs must ignore new service tables, while new scenario replay detects tampering in them.

## Task 1: Scenario-aware store and seeded service state

**Files:** Modify `internal/store/store.go`, `internal/store/store_test.go`.
**Interfaces:** Produce `(*Store).SeedScenario(ctx context.Context, seed int64, digest, scenario string) (World,error)` and keep `Seed` as a `duplicate-charge` wrapper. Add subscription, CRM, ticket, and messaging tables with `world_id` keys.

- [ ] Add failing tests: `SeedScenario(...,"company-incident")` creates C-104-linked account and incident signal, `company-routine` has no incident signal, `company-no-duplicate` omits CH-1002, and two worlds with equal seed have independent rows. Run `go test ./internal/store -run 'TestSeedCompany|TestCompanyWorldIsolation' -count=1` and observe the missing API failure.
- [ ] Implement schema and seed rows inside the existing transaction. Keep deterministic IDs/timestamps. Reject unknown scenarios. Run the focused tests, then `go test ./internal/store -count=1`.
- [ ] Commit as `Add scenario-aware company world state`.

## Task 2: Explicit company operation compilation

**Files:** Modify `internal/compiler/compiler.go`, `internal/compiler/compiler_test.go`; create `examples/company/openapi.yaml` and `examples/company/bindings.yaml`.
**Interfaces:** `Compile` and `ValidateManifest` accept precisely the contract map above plus the existing billing operations. The example compiles into one sorted, digested manifest.

- [ ] Add failing tests that compile the company example, reject an unknown binding, wrong route, wrong required/property type, and duplicated operation ID. Run `go test ./internal/compiler -run TestCompileCompany -count=1` and observe failure.
- [ ] Add exact contract definitions and company example. Keep the original billing YAML and digest unchanged. Run focused and package tests.
- [ ] Commit as `Compile explicit company service operations`.

## Task 3: CRM stateful handler

**Files:** Create `internal/crm/crm.go`, `internal/crm/crm_test.go`; modify `internal/dispatch/dispatch.go`, `internal/dispatch/dispatch_test.go`.
**Interfaces:** `crm.Handle(ctx,tx,worldID,runID,callID,behavior,args) (status int, body any, mutation any, err error)`; dispatch routes only `crm.` behaviors there.

- [ ] Add failing handler/dispatcher tests: search and get read seeded accounts; note and status changes are visible on subsequent reads; wrong account is 404; invalid status is 400; a repeated stable call ID creates one note; faulted first call creates none. Run focused tests and observe failure.
- [ ] Implement CRM reads and transaction-local writes. Return deterministic note IDs from run ID and call ID and a mutation payload for each write. Run `go test ./internal/crm ./internal/dispatch -count=1`.
- [ ] Commit as `Add transactional CRM behavior`.

## Task 4: Ticketing stateful handler

**Files:** Create `internal/ticketing/ticketing.go`, `internal/ticketing/ticketing_test.go`; modify `internal/dispatch/dispatch.go`, `internal/dispatch/dispatch_test.go`.
**Interfaces:** `ticketing.Handle` uses the same signature as CRM and routes `ticket.` bindings. Issue/comment IDs derive from run ID and call ID.

- [ ] Add failing tests: create/get/search issue, add/read comment, valid open to in_progress to resolved transition, invalid transition conflict, unknown account/project 404, idempotent create, and world isolation. Run focused tests and observe failure.
- [ ] Implement inside the caller's transaction and emit typed mutations. Run `go test ./internal/ticketing ./internal/dispatch -count=1`.
- [ ] Commit as `Add transactional ticketing behavior`.

## Task 5: Messaging stateful handler

**Files:** Create `internal/messaging/messaging.go`, `internal/messaging/messaging_test.go`; modify `internal/dispatch/dispatch.go`, `internal/dispatch/dispatch_test.go`.
**Interfaces:** `messaging.Handle` uses the same signature and routes `messaging.` bindings. Message ID derives from run ID and call ID.

- [ ] Add failing tests: list channels, read current messages, post then read, unknown channel 404, no duplicate post for repeated call ID, and no cross-world visibility. Run focused tests and observe failure.
- [ ] Implement inside the caller's transaction and emit a mutation on post. Run `go test ./internal/messaging ./internal/dispatch -count=1`.
- [ ] Commit as `Add transactional messaging behavior`.

## Task 6: Cross-service scenarios and deterministic evaluation

**Files:** Modify `internal/agent/scripted.go`, `internal/eval/eval.go`, `cmd/twinwright/main.go` and their tests; add a public `docs/adr/0005-company-world.md`.
**Interfaces:** CLI accepts `company-incident`, `company-routine`, `company-no-duplicate` with the company manifest; scripted provider receives the saved scenario; evaluator dispatches by scenario and reads actual rows.

- [ ] Add failing CLI tests that build the company manifest, run each scenario with the scripted fixture, pause/resume at least one, inspect, and assert state-based evaluation. Include a wrong manifest and no-refund case. Observe red tests.
- [ ] Wire scenario-aware seeding and provider selection. Implement scripted trajectories based on observed tool outputs. Add evaluation checks for refund correctness, CRM note, required/forbidden ticket/message, and unrelated-account/world rows. Run focused tests and full `go test ./...`.
- [ ] Commit as `Run and evaluate company scenarios`.

## Task 7: Verification replay for company scenarios

**Files:** Modify `internal/replay/replay.go`, `internal/replay/replay_test.go`, CLI replay tests.
**Interfaces:** Replay uses `SeedScenario` with saved scenario. Old `duplicate-charge` compares billing tables; company scenarios compare all service tables, including comments and messages.

- [ ] Add failing tests that verify a completed company run, detect a changed CRM note, issue, and message, confirm source DB is unchanged, and verify a pre-existing billing run. Observe red tests.
- [ ] Extend replay scenario support and table comparisons with deterministic sort keys. Run replay tests, full tests, vet, race detector, and CLI smoke run.
- [ ] Commit as `Verify multi-service runs by replay`.

## Task 8: Public documentation and final review

**Files:** Modify `README.md` and `docs/adr/0005-company-world.md`; verify `examples/company/openapi.yaml` and `examples/company/bindings.yaml`.

- [ ] Document actual company CLI commands, checks, and limitations; keep roadmap features separate from implemented features. Check README links.
- [ ] Run `gofmt`, `go vet ./...`, `go test -count=1 ./...`, `go test -race ./...`, build CLI, scripted scenario smoke runs, and replay for each variant. Inspect `git diff`, secret scan, and unsupported-feature behavior.
- [ ] Request independent code review; fix important findings, re-run affected checks, and commit documentation.