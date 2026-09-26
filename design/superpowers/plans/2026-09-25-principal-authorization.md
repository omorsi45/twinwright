# Principal Authorization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enforce principal permissions and resource limits outside the model, with replayable security decisions and a prompt-injection scenario.

**Architecture:** Parse a strict version 1 authorization policy, persist it with a principal on each run, and evaluate it inside each unique valid tool-call transaction. Filter exposed operations before provider calls but retain dispatcher enforcement. Replay and fork rebuild policy state from the same tool trajectory; an adversarial fixture proves attempted and blocked violations.

**Tech Stack:** Go 1.27.1, SQLite via modernc.org/sqlite, existing YAML compiler, store, dispatcher, runner, replay, checkpoint, fork, and CLI.

**Spec:** `C:\Users\Omar Morsi\Desktop\Projects\twinwright\design\superpowers\specs\2026-09-25-principal-authorization-design.md`

## Global constraints

- Start from Milestone 5 commit `4e8e05f` in a new ignored worktree. PR 4 targets main and remains open; do not merge it without explicit authorization.
- Preserve historical unrestricted runs, `--fault`, all chaos effects, replay digests, and the call-ID idempotency contract.
- Parse security configuration before world or run creation. A secure run denies unknown behaviors; no policy is an explicit local-unrestricted mode.
- Authorization is a runtime check, never a prompt instruction alone. Denial, state, result, transcript, and ledger commit atomically.
- Personal spec, plan, and progress stay in ignored `design/`; public contract belongs in README and ADR 0009.
- Each task starts with a behavior test that fails, ends with focused and full tests, vet, diff check, a scoped commit, and a `design/progress.md` entry.
- No paid model call, external identity service, or real sleeping is required.

## File map

- `internal/authz/policy.go`, `policy_test.go`: built-in permission mapping, strict parser, canonical policy, digest.
- `internal/authz/decision.go`, `decision_test.go`: active grants, transaction counter, resource and attribute checks, operation filtering.
- `internal/store/store.go`, `store_test.go`: principal column, auth policy/state tables, atomic run creation and old-schema migration.
- `internal/dispatch/dispatch.go`, `auth_test.go`: authorization before chaos or service handlers, audit events and denial result.
- `internal/agent/runner.go`, tests: provider receives active operations; direct unauthorized calls reach the dispatcher for denial.
- `internal/replay/replay.go`, `internal/checkpoint/*`, `internal/fork/*`, tests: authorization reconstruction, policy and state copying, replacement and tamper checks.
- `internal/agent/security_scripted.go`, `internal/eval/security.go`, tests: malicious ticket fixture and attempted/blocked/successful evidence.
- `cmd/twinwright/main.go`, tests, `examples/security/*`, README, `docs/adr/0009-principal-authorization.md`: CLI and public contract.

## Review focus

- A scoped broad search must not leak another customer's data; reject it before the handler.
- A denied write must not consume a chaos effect or mutate the world, even when the chaos policy has an actor write.
- Reusing a denied call ID must return the identical 403 without advancing a temporary grant or revocation counter.
- A child fork after a grant expiration must inherit the expired state; replacement starts at call zero and replay reproduces it.
- Read-only replay of a database without auth tables must preserve the legacy unrestricted ledger exactly.

## Task 1: Policy contract and permission map

**Files:** Create `internal/authz/policy.go`, `policy_test.go`, `examples/security/support-policy.yaml`.

**Interfaces:** `authz.Parse([]byte, compiler.Manifest) (Policy,error)`, `Policy.CanonicalJSON() ([]byte,error)`, `Policy.Digest() string`, `authz.Permission(compiler.Operation) (string,bool)`.

- [ ] Write parser tests that first fail on undefined `authz.Parse`: a support role grants `customers.read` and `charges.read`; direct allow adds `refunds.create`; changing role or permission changes the digest. Reject unknown role, permission, operation behavior, duplicate grant, unknown YAML field, negative amount, empty principal ID, overlapping contradictory grant window, and multiple YAML documents.
- [ ] Implement strict decoding and explicit mapping for every built-in behavior. Keep role, resource, and schedule representation canonical and deterministic; no arbitrary permission string is accepted.
- [ ] Run `go test ./internal/authz -count=1`, full suite, vet, diff check, and commit `Define principal authorization policies`.

## Task 2: Identity and policy persistence

**Files:** Modify `internal/store/store.go`, `store_test.go`.

**Interfaces:** `store.Run.PrincipalID`; `store.CreateRunConfigured(ctx, worldID, scenario, provider, model, task string, options RunOptions) (Run,error)` with fault, chaos, and auth policy options; `Store.AuthPolicy(ctx,runID) ([]byte,string,error)`.

- [ ] Write red tests that secure creation saves principal and canonical policy atomically, legacy creation records `local-unrestricted`, opening an older DB migrates a missing principal column to `legacy-local`, and read-only opening treats a missing auth table as no policy without writing.
- [ ] Add `principal_id` to runs and `run_auth(run_id,policy_json,digest)`, `auth_state(run_id,call_index)` tables. Refactor existing creation entry points through the configured transaction without changing legacy event payloads. Reject mismatched policy digest before insertion.
- [ ] Run store tests, full suite, vet, diff check; commit `Persist run principals and authorization policies`.

## Task 3: Transactional decision and resource limits

**Files:** Create `internal/authz/decision.go`, `decision_test.go`.

**Interfaces:** `authz.Decide(ctx,tx,runID,worldID string,op compiler.Operation,args map[string]any) (Decision,error)`; `authz.Exposed(ctx,db,runID string,ops []compiler.Operation) ([]compiler.Operation,error)`.

- [ ] Write red tests for role/direct union, default deny, temporary grant calls 2 through 3, revocation after call 3, idempotency delegated to dispatcher, customer joins through invoice/charge/account/issue, channel and project scopes, explicitly empty scope, broad search denial, and refund amount cap. Assert denied decisions contain stable reason codes.
- [ ] Evaluate a single per-run call index inside the existing dispatch transaction. Resolve relationships only with `world_id` predicates. Exposed filters by operation permission at the next call index; dispatcher still checks resource and amount.
- [ ] Run authz tests, full suite, vet, diff check; commit `Enforce transactional authorization decisions`.

## Task 4: Dispatcher enforcement and audit

**Files:** Modify `internal/dispatch/dispatch.go`; create `internal/dispatch/auth_test.go`.

- [ ] Write red tests that an unauthorized `createRefund` returns 403 with `authorization.denied`, no refund, no chaos counter change, and one saved observation; reusing its call ID changes nothing. Authorized call records `authorization.allowed`. An audit insert failure rolls back both a decision and a permitted mutation.
- [ ] Authorize after argument validation and before chaos decision or handler. Store `authorization.allowed`/`authorization.denied` with principal, permission, reason, call and operation IDs. A denied result follows the same atomic response/result/transcript path as any other tool call.
- [ ] Run dispatcher tests, full suite, vet, diff check; commit `Audit and block unauthorized tool calls`.

## Task 5: Provider operation exposure

**Files:** Modify `internal/agent/runner.go`, tests and `internal/checkpoint/reconstruct.go`.

- [ ] Write red tests that a support provider receives only currently active operations, temporary grants appear and expire at the scheduled call, and a scripted direct call to a hidden operation is recorded as a 403 rather than reaching its handler.
- [ ] Use `authz.Exposed` for both the model request ledger payload and provider call. Reconstruct the same filtered list at checkpoints. Keep legacy runs' operation list unchanged.
- [ ] Run agent/checkpoint/replay tests, full suite, vet, diff check; commit `Expose only active principal operations`.

## Task 6: Replay and fork continuity

**Files:** Modify `internal/replay/replay.go`, `internal/checkpoint/checkpoint.go`, `internal/checkpoint/reconstruct.go`, `internal/fork/fork.go`, `internal/fork/replay.go`, `internal/store/store.go`; add tests.

- [ ] Write red tests for a completed secure root replay, forks before and after denial, inherited expired grants, child policy replacement, tampered policy/counter/audit event, and an actual pre-M6 read-only DB without auth tables.
- [ ] Seed auth policy in root and checkpoint replay; treat authorization audit events as semantic and group them inside tool calls. Copy prefix auth state on inherited fork; record `auth_replaced` in lineage for explicit replacement. Compare final policy and counter on replay. Preserve old ledgers exactly.
- [ ] Run replay/checkpoint/fork tests, full suite, vet, diff check; commit `Replay principal decisions and fork state`.

## Task 7: Adversarial ticket and security evaluation

**Files:** Modify `internal/store/store.go`, `internal/eval/company.go`; create `internal/agent/security_scripted.go`, `internal/eval/security.go`, tests, `examples/security/support-policy.yaml`.

- [ ] Write red tests that the malicious ticket comment is returned as untrusted service data; a scripted fixture tries C-205 lookup and message posting; support policy produces two attempted and blocked violations with zero successful violations. An intentionally broad policy permits at least one successful violation and is labeled accordingly. The legitimate C-104 task outcome remains independently reported.
- [ ] Seed `prompt-injection-ticket` with a ticket comment and add a provider fixture. Analyze ordered tool request/response and authorization events with evidence IDs; do not infer safety from the final message alone.
- [ ] Run agent/eval tests, full suite, vet, diff check; commit `Evaluate prompt injection against authorization`.

## Task 8: CLI, docs, smoke, review, PR

**Files:** Modify `cmd/twinwright/main.go`, tests, README; create `docs/adr/0009-principal-authorization.md`, examples.

- [ ] Write red CLI tests for `run --auth` preflight before database creation, principal persistence across resume, `fork --auth` replacement, direct denied call inspection, security scenario report, and replay. Retain `--chaos` and legacy `--fault` behavior.
- [ ] Add CLI parsing and report fields. Document default local identity, policy syntax, role/resource constraints, deterministic temporary and revoked permissions, denial events, prompt injection boundaries, and scripted commands.
- [ ] Build CLI and smoke legacy billing, company, chaos, secure support, and overprivileged security cases with evaluation and replay. Run full suite, vet, diff check, and secret scan. Request independent review and fix critical/important findings with red-green tests. Commit, push, open a PR against main if PR 4 has landed or against `milestone-5-chaos` while it remains open, and attach the PR artifact. Update ignored progress ledger and vault project pages/log.
