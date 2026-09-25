# Twinwright

Twinwright runs agents against local, stateful simulations of business software. Milestone 1 compiles a constrained fictional billing OpenAPI specification plus explicit behavior bindings, then runs a duplicate-charge investigation against seeded SQLite state. A refund changes subsequent charge reads. Every run has an ordered event ledger and can pause and resume.

## Requirements

- Go 1.27 or newer
- An OpenAI API key and model name for live model runs

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
```

The default database is `twinwright.db`; it is ignored by Git. Each `run` creates a new isolated world instance, even when the same seed is used. `inspect` shows ledger events and four deterministic state checks.

For a live agent, set `OPENAI_API_KEY` and choose a model:

```powershell
$env:OPENAI_API_KEY = 'your-key'
go run ./cmd/twinwright run duplicate-charge --agent openai --model <model-name> --seed 42 --fault listCharges
```

The OpenAI adapter uses the [Responses API](https://developers.openai.com/api/docs/guides/function-calling) with function tools. Live results depend on model behavior and have not been exercised without a key; the adapter is covered by a local HTTP test. The evaluator trusts only SQLite state, never the agent's final message.

## Scope

The compiler supports exactly five fictional billing operations and rejects other routes or unbound operations. OpenAPI defines callable shapes; [bindings](examples/billing/bindings.yaml) choose explicit stateful behaviors. This milestone has no production billing connection, permissions, generic service generation, distributed workers, or replay engine. The [Phase 0 design](docs/superpowers/specs/2026-09-24-billing-world-design.md), [ADRs](docs/adr), and [implementation plan](docs/superpowers/plans/2026-09-24-billing-world.md) describe the boundaries and next risks.
