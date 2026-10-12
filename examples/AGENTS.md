# Versioned fixture instructions

Applies to `examples/` and all its nested fixture directories.

- These files are runnable scenarios and test evidence, not illustrative
  snippets. Preserve schema versions, operation IDs, deterministic seeds,
  and references between contracts, behavior bindings, world definitions,
  assertions, and policies.
- The billing fixtures are in `billing/`; `company/` includes a four-service
  world and `company/services/` contracts and bindings. Other directories
  hold chaos, security, benchmark, container, counterfactual, and shadow
  fixtures.
- Use fictitious data only. Do not include real customer records,
  credentials, external hosts, or production endpoints.
- For changed fixture semantics, extend the consuming Go tests and run the
  deterministic end-to-end script. Do not merely change expected output
  to hide a regression.
- Keep shadow observation exports valid and representative, including
  their read-only configuration constraints.

From the root: `./scripts/e2e.sh` and `go test -count=1 ./...`.
