# Development

This page was moved out of the project README when the README was rewritten as the Twinwright Cloud overview. It holds the README section "Development", unchanged except where noted. Paths in backticks are relative to the repository root. [Back to the guide index](README.md).

## Development

```bash
git clone https://github.com/omorsi45/twinwright
cd twinwright
go test ./...        # hermetic: no network, no API key, no container runtime
./scripts/e2e.sh     # the whole documented pipeline, with scripted fixtures
```

`make help` lists the rest. The gate CI runs:

```bash
make verify          # gofmt, vet, build, tests, end-to-end
make test-race       # the worker runtime is concurrent
docker compose up -d && make verify-full   # adds the PostgreSQL integration tests
```

PostgreSQL tests are opt-in behind `TWINWRIGHT_TEST_POSTGRES_DSN` and skip when
it is unset, so the default suite stays hermetic.

`CONTRIBUTING.md` describes what a change is expected to prove.

The minimal billing world is the smaller reference example: `build` it, `run`
`duplicate-charge` with `--fault listCharges`, then `resume`, `inspect` and `replay`
it exactly as the sections above do for the company world. The default SQLite
database is `twinwright.db` and is ignored by Git.
