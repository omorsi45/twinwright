# Distributed worker instructions

Applies to `internal/worker/` in addition to its ancestors' instructions.

- A run is delivered **at least once**. Recovery after a worker dies may
  repeat execution, so depend on idempotent tool effects and guarded writes.
- Maintain claim, lease renewal, expiry, takeover, backoff, and max-attempt
  semantics. A worker that loses its fence must stop committing.
- The execution context is canceled on lost ownership. Preserve cancelation
  propagation to provider and dispatcher boundaries.
- Never insert worker IDs, lease renewals, or ownership transitions into
  the agent event ledger. Those belong in dedicated storage tables.
- Test simultaneous workers, crashed-owner takeover, stale-commit rejection,
  and cancellation using controllable clocks where possible.

From the root:
```sh
go test -race -count=1 ./internal/worker ./internal/store
make test-postgres   # only with PostgreSQL available
```

See `docs/distributed.md` and ADRs 0018 through 0020.
