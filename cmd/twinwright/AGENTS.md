# CLI instructions

Applies to `cmd/twinwright/`. Read the root `AGENTS.md` first.

- Keep CLI command names, flags, error handling, and usage aligned with
  `README.md`, `scripts/e2e.sh`, and the examples actually shipped.
- Changes to `run`, `resume`, `worker`, `replay`, `fork`, or `evaluate`
  must preserve the internal package's transaction and read-only guarantees.
- The `demo` walkthrough is an executable twelve-step claim. Do not make a
  step print success without validating its own evidence.
- No live provider credentials should be needed for a CLI smoke or demo test.
- Add or update `*_test.go` for flags, exit codes, and compatibility.

Newer commands, and what has to stay aligned with them:

- `gate`: its exit codes are a CI contract, 0 for no regressions, 1 for
  regressions and 2 when it cannot decide, and the regression kind names are part
  of it (ADR 0030). Keep `--baseline`, `--candidate`, `--tolerance`,
  `--trajectory`, `--markdown` and `--junit` in line with `docs/guide/bench.md`,
  the `regression-gate` job in `.github/workflows/ci.yml`, `make gate` and
  `TestRegressedBenchFailsTheGate`. The gate must stay able to fail: the CI job
  requires exit 1 on `--model fixture-regressed-v1`.
- `run --fail-on-eval`: opt-in, and refused with `--enqueue`.
- `perf`: writes its report before it exits 1 on a duplicated mutation, a replay
  that did not verify or a crash-matrix violation, exits 2 on bad input, and never prints a DSN. The
  throughput table in `docs/performance.md` is held to the committed reports in
  `docs/benchmarks/` by `TestBenchmarkDocsMatchTheirReports`.
- `run --record-cassette` and `replay --cassette`: the recording flag exists on
  `run` only and is refused for the scripted provider, for `exec` and with
  `--enqueue`. A cassette holds a model's prompts and answers, so nothing a test
  or a script writes may land in the repository tree. Keep the wording in
  `docs/guide/providers.md` and ADR 0031 in line.
- `--agent exec` with `--agent-command`, `--agent-env` and the `--agent-container`
  family: the child environment is `PATH`, `HOME` and the named variables only, a
  `NAME=value` is refused, and the command is not stored with the run, so `resume`
  and `worker` need it again. Commands that take a run's provider from its record
  (`worker`, `resume`, `fork`, `counterfactual`) use `providerConfig.forRun`, which
  applies the agent flags to exec runs only; commands that name the provider use
  `with` and refuse them for any other. Keep the flag help, `docs/protocol/agent-v1.md` and
  its JSON Schemas, `exec_test.go`, `exec_fleet_test.go` and ADR 0033 in line.
- `make demo` and `make demo-stack`: `make demo` builds the CLI and runs the
  twelve-step `demo` in a temporary directory, and `ci.yml` runs it. `make
  demo-stack` runs `scripts/demo-stack.sh` against Compose, and the `demo-stack`
  job in `integration.yml` runs that. Keep the Makefile targets, the README quick
  start and those two jobs saying the same thing.
- `docs/claims.md` cites tests and CI jobs by name. Renaming or deleting a cited
  test, or a workflow job, fails `TestClaimsTableReferencesExist` until the row is
  updated, and a changed relative link fails `TestDocLinksResolve`.

From the repository root, test with:
```sh
go test -count=1 ./cmd/twinwright
go run ./cmd/twinwright demo --dir "$(mktemp -d)/twinwright-demo"
make demo
./scripts/e2e.sh
```
