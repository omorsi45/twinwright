# Development script instructions

Applies to `scripts/`.

- `e2e.sh` is a deterministic, scripted-provider validation of the
  documented CLI workflow. Keep the default SQLite mode network-free,
  API-key-free, and container-free.
- Optional PostgreSQL execution takes its DSN as an argument. Do not
  hardcode database passwords or leak DSNs into committed output.
- Use `set -euo pipefail`, temporary working directories, and cleanup
  traps. Check exit codes and evidence, not just text that claims success.
- `perf.sh` measures local runtime primitives. Preserve provenance,
  fixed iteration counts, and the distinction between measured and
  unmeasured figures described in `docs/performance.md`.
- A script change needs its relevant smoke run and must not delete
  developer data outside its own temporary workspace.

From the root: `./scripts/e2e.sh` or `make bench-smoke`, as relevant.
