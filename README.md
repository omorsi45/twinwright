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
go run ./cmd/twinwright replay <run-id>
```

The default database is `twinwright.db`; it is ignored by Git. Each `run` creates a new isolated world instance, even when the same seed is used. `inspect` shows ledger events and four deterministic state checks.

Replay verifies a completed run by executing its recorded assistant decisions in a fresh in-memory billing world. It makes no model call and leaves the source database unchanged. A successful report includes model, tool, and event counts. On a difference, it emits a JSON report with the first divergence and exits nonzero. Replay currently supports completed duplicate-charge runs with valid model turns; it does not support counterfactual changes or historical runtime versions.

Keep the compiled manifest for resume and replay: its digest must match the world used by the run. A run also saves its provider model and uses that model on resume. If execution fails after a run starts, the error includes the run ID so it can be inspected or resumed. Older development databases without the model column are updated when opened.

For a live agent, set `OPENAI_API_KEY` and choose a model:

```powershell
$env:OPENAI_API_KEY = 'your-key'
go run ./cmd/twinwright run duplicate-charge --agent openai --model <model-name> --seed 42 --fault listCharges
```

The OpenAI adapter uses the [Responses API](https://developers.openai.com/api/docs/guides/function-calling) with function tools. Live results depend on model behavior and have not been exercised without a key; the adapter is covered by a local HTTP test. The evaluator trusts only SQLite state, never the agent's final message.

## Scope

The compiler supports exactly five fictional billing operations and rejects other routes or unbound operations. OpenAPI defines callable shapes; [bindings](examples/billing/bindings.yaml) choose explicit stateful behaviors. The current scope has no production billing connection, permissions, generic service generation, distributed workers, or general replay engine. The [Phase 0 design](docs/superpowers/specs/2026-09-24-billing-world-design.md), [replay design](docs/superpowers/specs/2026-09-25-replay-verification-design.md), and [ADRs](docs/adr) describe the boundaries and next risks.
