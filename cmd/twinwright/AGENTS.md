# CLI instructions

Applies to `cmd/twinwright/`. Read the root `AGENTS.md` first.

- Keep CLI command names, flags, error handling, and usage aligned with
  `README.md`, `scripts/e2e.sh`, and the examples actually shipped.
- Changes to `run`, `resume`, `worker`, `replay`, `fork`, or `evaluate`
  must preserve the internal package's transaction and read-only guarantees.
- The `demo` walkthrough is an executable eleven-step claim. Do not make a
  step print success without validating its own evidence.
- No live provider credentials should be needed for a CLI smoke or demo test.
- Add or update `*_test.go` for flags, exit codes, and compatibility.

From the repository root, test with:
```sh
go test -count=1 ./cmd/twinwright
go run ./cmd/twinwright demo --dir "$(mktemp -d)/twinwright-demo"
./scripts/e2e.sh
```
