# World compiler instructions

Applies to `internal/compiler/` in addition to its ancestors' instructions.

- OpenAPI determines operation shapes; behavior bindings determine effects.
  Do not infer business semantics solely from a schema.
- Keep world manifests and digests deterministic for identical inputs.
  Validate malformed or inconsistent definitions before runtime execution.
- Preserve the documented separation between a service contract, bindings,
  and a multi-service world definition.
- Extend tests for compilation failures as well as valid billing and
  company-world fixtures in `examples/`.

Run from the root: `go test -count=1 ./internal/compiler`.
