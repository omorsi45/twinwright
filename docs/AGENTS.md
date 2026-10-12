# Documentation instructions

Applies to `docs/`, including `docs/adr/`.

- The source of truth for implemented behavior is code plus passing tests.
  Check both before changing a security, isolation, performance, or
  distributed-runtime claim.
- Keep ADRs as records of decisions and trade-offs. When an invariant changes,
  add an ADR instead of silently rewriting why an older decision was made.
- `security-review.md` pairs each claim with code and adversarial test
  evidence. Clearly say when a guarantee is structural but untested, or
  when external systems were never exercised.
- `distributed.md` must match real tables and fencing semantics; it is
  checked by `internal/store/diagrams_test.go`.
- `performance.md` must report measured environment, commit, and iteration
  provenance. Do not invent or extrapolate benchmarks.
- Keep documentation examples executable and aligned with the CLI, fixtures,
  and `scripts/e2e.sh`.

For changed code examples or runtime claims, run the appropriate package
tests and `./scripts/e2e.sh` from the root.
