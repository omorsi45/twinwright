# GitHub automation instructions

Applies to `.github/`, including `.github/workflows/`.

- `ci.yml` is a deterministic fast lane: formatting, vet, build, unit tests,
  benchmark smoke, module tidy, end-to-end demo, race detection, gitleaks,
  and govulncheck. Keep provider keys empty.
- `integration.yml` is where real PostgreSQL 16/17 and Docker daemon tests
  run. Do not move credentials, live-model calls, or nondeterministic
  external dependencies into the fast lane.
- Beyond `postgres` and `docker`, `integration.yml` has `real-model` (a local
  Ollama model, recorded and replayed, verdict reported and not gated),
  `observability` (spans read back from Jaeger, a worker scraped by
  Prometheus), `perf` (PostgreSQL 17 benchmarks, `workflow_dispatch` only) and
  `demo-stack` (`make demo-stack`, which fails when any of its six checks is
  missing). None of them calls a hosted provider or holds a key.
- Do not disable security scans, downgrade test coverage, remove race checks,
  or mask failure exit codes to make CI pass. Fix the failure or document a
  narrowly justified change.
- Keep Go setup tied to `go.mod`; when adjusting workflow commands,
  verify they match `Makefile` and `CONTRIBUTING.md`.
- Check both workflow results on pull requests before declaring
  integration verified. Never present a skipped optional integration as
  a passing test.
