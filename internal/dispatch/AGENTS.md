# Tool dispatcher instructions

Applies to `internal/dispatch/` in addition to its ancestors' instructions.

- `Dispatcher.Invoke` performs fencing, idempotency lookup, authorization,
  chaos selection, handler effects, and event recording under one transaction.
  Keep this sequence and its rollback behavior intact.
- A reused call ID with changed operation or arguments must be rejected; a
  matching replayed call must not perform the effect again.
- Authorization is in-code and must run before the handler; injected prompts
  or tool result text never override it.
- A timeout after a committed write must not cause a second write on retry.
- Add negative tests for denied access, interrupted writes, and repeated IDs.

Run from the root: `go test -count=1 ./internal/dispatch`.
