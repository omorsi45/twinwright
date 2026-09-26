# Contributing

Twinwright is a testing harness for autonomous agents, so its own correctness
claims have to hold up. The conventions below exist for that reason, not for
ceremony.

## Getting set up

```bash
git clone https://github.com/omorsi45/twinwright
cd twinwright
go test ./...        # hermetic: no network, no API key, no container runtime
./scripts/e2e.sh     # walks the whole documented pipeline with scripted fixtures
```

Nothing beyond a Go toolchain is required. PostgreSQL and an OpenTelemetry
collector are optional:

```bash
docker compose up -d
make test-postgres
make e2e-postgres
```

`make help` lists the rest.

## What good change looks like here

**Every reliability claim needs an adversarial test.** The useful question is not
"does the happy path work" but "if I delete this safety check, does a test go
red?" If the answer is no, the test is decoration. Several tests in this
repository were written by first breaking the thing they protect and confirming
the failure: the fencing tests, the crash-recovery tests, and the model-request
reuse in `internal/agent/runner.go` were all verified that way.

**Tests come before implementation.** Write the failing test, watch it fail for
the reason you expect, then make it pass. A test that has never failed has not
been shown to test anything.

**Do not weaken a check to make a test pass.** No `--no-verify`, no skipped
assertions, no swallowed errors. If a check is wrong, change the check
deliberately and say why in the commit message.

**State what you did not verify.** "Container tests were not run because no
daemon was available" is a useful sentence. Claiming a test passed when it was
skipped is the one unrecoverable mistake in a repository about trustworthy
execution.

## Guarantees you must not regress

These hold today and are tested. If a change alters one, that needs an ADR in
`docs/adr/`, not a commit message:

- Worlds are isolated by `world_id`; a mutation in one seed instance never
  reaches another.
- Tool calls are idempotent by call ID. The same call ID with different
  arguments is refused, not answered from cache.
- Local mutations commit atomically with their ledger events.
- Replay never writes to the database it is verifying.
- The run ledger records what the *agent* did. Ownership, worker identity and
  fencing do not belong in it: fork lineage and checkpoint reconstruction digest
  that prefix, so an identical trajectory must produce an identical ledger no
  matter which worker executed it.
- Distributed delivery is **at-least-once**. Nothing may claim exactly-once.
- Authorization is enforced in code, inside the tool transaction, before any
  handler runs. Never in a prompt.

## Before you open a pull request

```bash
make verify       # gofmt, vet, build, tests, end-to-end
make test-race    # the worker runtime is concurrent; this matters
```

With PostgreSQL running, `make verify-full` adds the storage and distributed
integration tests. CI runs the same commands; running them locally first just
shortens the loop.

Also check `git status` for artifacts that should not be committed: generated
`*.db` files, manifests, benchmark reports, local logs.

## Commit and pull request style

Commits explain *why*, in prose, wrapped at 72 characters. The diff already shows
what changed. A commit that fixes a bug should say what the bug was and how the
new test catches it.

Milestone-sized work belongs in several coherent commits, not one large one.

## Where the design lives

`docs/adr/` holds the architecture decisions, including the alternatives that
were rejected and why. ADR 0017 froze the single-node guarantees; 0019 and 0020
map them onto PostgreSQL and the multi-worker runtime. Read those two before
changing storage or execution.

## Reporting a security issue

See `SECURITY.md`. Please do not open a public issue for a vulnerability.
