# Twinwright

Twinwright runs agents against local, stateful simulations of business software. Its first milestone is a fictional billing world compiled from a small OpenAPI description and explicit behavior bindings.

The first slice is deliberately narrow: one provider, one scenario, one local database, and one controlled failure. See [the design](docs/superpowers/specs/2026-09-24-billing-world-design.md) and [implementation plan](docs/superpowers/plans/2026-09-24-billing-world.md).

## Milestone 1 commands

```text
twinwright build examples/billing/openapi.yaml
twinwright run duplicate-charge --agent openai --seed 42
twinwright resume <run-id> --agent openai
twinwright inspect <run-id>
```

Run `twinwright --help` for current options. The example is fictional; it does not connect to a real billing system.
