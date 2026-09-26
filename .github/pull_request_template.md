## What changed and why

<!-- The diff shows what. Explain why, and what the alternative was if there
     was a real choice to make. -->

## Verification

<!-- Paste the commands you ran, not a claim that you ran them. -->

```
make verify
```

- [ ] `gofmt`, `go vet`, `go build`, `go test ./...`
- [ ] `go test -race ./...` (required for any change to the worker runtime,
      leases, or the dispatcher)
- [ ] `./scripts/e2e.sh`
- [ ] PostgreSQL integration tests (`make test-postgres`), if storage changed
- [ ] `git status` is clean of generated databases, manifests and logs

## Adversarial test

<!-- Which new or existing test fails if the safety property this change relies
     on is removed? If none, the change is not yet covered. -->

## Guarantees

- [ ] No regression to world isolation, call-ID idempotency, atomic mutation,
      replay source safety, or in-code authorization
- [ ] Nothing in this change claims exactly-once delivery
- [ ] Ownership and worker identity stay out of the run ledger
- [ ] An ADR is added or updated if a documented guarantee changed

## Not verified

<!-- State plainly what you could not run and why: no Docker daemon, no provider
     credentials, no PostgreSQL. This section being non-empty is normal and is
     far better than an unsupported claim. -->
