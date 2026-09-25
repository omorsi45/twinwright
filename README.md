# Twinwright

Twinwright runs agents against local, stateful simulations of business software. It compiles constrained fictional OpenAPI specifications plus explicit behavior bindings, then runs billing and company support scenarios against seeded SQLite state. Service writes change subsequent reads. Every run has an ordered event ledger and can pause and resume.

## Requirements

- Go 1.27 or newer
- An OpenAI API key for live model runs

No key is needed for the deterministic scripted example. The scripted agent is a test fixture; it is not a model integration.

## Try the example

Run from the repository root:

```powershell
go test ./...
go run ./cmd/twinwright build examples/billing/openapi.yaml
go run ./cmd/twinwright run duplicate-charge --agent scripted --seed 42 --fault listCharges --steps 3
```

The `run` command prints a JSON run ID and pauses after three model turns. Then resume and inspect it:

```powershell
go run ./cmd/twinwright resume <run-id> --agent scripted
go run ./cmd/twinwright inspect <run-id>
go run ./cmd/twinwright replay <run-id>
```

The default database is `twinwright.db`; it is ignored by Git. Each `run` creates a new isolated world instance, even when the same seed is used. `inspect` shows ledger events and four deterministic state checks.

Replay verifies a completed run by executing its recorded assistant decisions in a fresh in-memory world. It makes no model call and leaves the source database unchanged. A successful report includes model, tool, and event counts. On a difference, it emits a JSON report with the first divergence and exits nonzero. Replay supports the billing and company example scenarios with valid model turns; forked-run replay and historical runtime versions are not yet supported.

## Try the company scenarios

Build the company manifest with its behavior bindings:

```powershell
go run ./cmd/twinwright build examples/company/openapi.yaml --bindings examples/company/bindings.yaml --out company.manifest.json
go run ./cmd/twinwright run company-incident --agent scripted --manifest company.manifest.json --db company.db --seed 42
```

Use the returned run ID to inspect and verify the result:

```powershell
go run ./cmd/twinwright inspect <run-id> --db company.db
go run ./cmd/twinwright replay <run-id> --manifest company.manifest.json --db company.db
```

The incident case refunds the duplicate charge, records a CRM note, opens an engineering issue, and posts to the support channel. `company-routine` has a duplicate charge without incident evidence, so it requires a refund and CRM note without a ticket or post. `company-no-duplicate` requires a CRM note with no refund or escalation. The note must describe the finding: duplicate charge and refund, or one legitimate charge and no refund. CRM status changes are optional. Replace the scenario name in the `run` command to try each variant. Add `--fault createRefund` to inject one temporary 503, or `--steps 4` to pause and resume. The evaluator checks persisted state, including the absence of unrelated account changes.

## Build a versioned world

The [company world definition](examples/company/world.yaml) assembles separate billing, CRM, ticketing, and messaging specifications:

```powershell
go run ./cmd/twinwright build-world examples/company/world.yaml --out company.world.manifest.json
go run ./cmd/twinwright run company-incident --agent scripted --manifest company.world.manifest.json --db company.db
go run ./cmd/twinwright replay <run-id> --manifest company.world.manifest.json --db company.db
```

The definition declares `version: 1`, a `company-v1` seed profile, service files, entity fields, and relationships such as `crm.accounts.customer_id` to `billing.customers.id`. Referenced files must stay under the definition directory. Each operation needs an explicit behavior binding; the compiler accepts only primitive GET path arguments or required POST JSON object arguments. New business behavior requires a registered Go handler, and relationship metadata does not create database constraints. The original `build` command and manifests remain supported. See [ADR 0006](docs/adr/0006-world-definition.md) for the boundary.

## Checkpoints and counterfactual forks

A paused or completed run can list committed restore boundaries. Choose an `event_seq` from the JSON output:

```powershell
go run ./cmd/twinwright checkpoints <run-id> --manifest company.world.manifest.json --db company.db
go run ./cmd/twinwright fork <run-id> --at-event <event-seq> --manifest company.world.manifest.json --db company.db
```

The fork command creates a separate world and a paused child run. It reconstructs and verifies the selected prefix before copying state. The parent is unchanged. Use `--steps 20` to continue immediately, or resume the returned child run later. `--agent`, `--model`, and `--fault` can change future execution while preserving prior observations. A new fault is applied only to the child.

```powershell
go run ./cmd/twinwright resume <child-run-id> --agent scripted --manifest company.world.manifest.json --db company.db
go run ./cmd/twinwright inspect <child-run-id> --db company.db
go run ./cmd/twinwright compare <parent-run-id> <child-run-id> --db company.db
```

`inspect` includes parent and checkpoint lineage for a fork. `compare` reports tool and mutation trajectories after the fork point, state row differences, and completed-run evaluation checks. Latency and model usage are marked unavailable because the current runtime does not record those measurements. Checkpoints require the same manifest digest and a paused or completed source; event sequences inside a tool transaction are rejected. See [ADR 0007](docs/adr/0007-checkpoint-fork.md).

Keep the compiled manifest for resume and replay: its digest must match the world used by the run. A run also saves its provider model and uses that model on resume. If execution fails after a run starts, the error includes the run ID so it can be inspected or resumed. Older development databases without the model column are updated when opened.

For a live agent, set `OPENAI_API_KEY`. The repo defaults to `gpt-6-sol`:

```powershell
$env:OPENAI_API_KEY = 'your-key'
go run ./cmd/twinwright run duplicate-charge --agent openai --seed 42 --fault listCharges
```

Override the default with `OPENAI_MODEL` or `--model`. Keep your API key outside the repository.

The OpenAI adapter uses the [Responses API](https://developers.openai.com/api/docs/guides/function-calling) with function tools. Live results depend on model behavior and have not been exercised without a key; the adapter is covered by a local HTTP test. The evaluator trusts only SQLite state, never the agent's final message.

## Scope

The legacy compiler supports five billing operations in the original example and 18 explicit billing, CRM, ticketing, and messaging operations in the [company example](examples/company/openapi.yaml). The versioned world compiler combines multiple service manifests while requiring registered bindings. OpenAPI defines callable shapes; [bindings](examples/company/bindings.yaml) choose explicit stateful behaviors. All services run locally in one process and one SQLite database. The current scope has no production service connections, permissions, dynamic behavior generation, or distributed workers. The [ADRs](docs/adr) describe the public architectural decisions and current limits.

## License

Apache License 2.0. See [LICENSE](LICENSE).
