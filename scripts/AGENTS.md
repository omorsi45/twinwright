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
- `live-model.sh` runs a local OpenAI-compatible model on the billing
  world, records its HTTP exchanges and replays the run from the ledger and
  the cassette. It is opt-in, needs a server and `TW_LIVE_MODEL`, never runs
  in `make verify` or the fast CI lane, and its outputs hold prompts and
  answers, so none of them is committed.
- `demo-stack.sh` (`make demo-stack`) runs the Compose stack, two workers and
  a takeover, and exits non-zero unless six checks read back from PostgreSQL,
  Jaeger and Prometheus hold. It drops and recreates only the
  `twinwright_demo_*` databases, never prints a DSN, and ends its workers and
  its lock session in an exit trap. The trap removes its temporary directory on
  success and keeps it, printing the path, on failure. It refuses to start when
  the metrics ports 9095 or 9096 already answer, and each worker's endpoint must
  serve that worker's own series before Prometheus is believed. PostgreSQL's host
  port is `TW_DEMO_PG_PORT` (default 5432), the variable `docker-compose.yml`
  reads too. It leaves the stack running.

From the root: `./scripts/e2e.sh` or `make bench-smoke`, as relevant.
