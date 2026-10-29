# Engineering guarantees

This page was moved out of the project README when the README was rewritten as the platform overview. It holds the README section "Engineering guarantees", unchanged except where noted. Paths in backticks are relative to the repository root. [Back to the guide index](README.md).

## Engineering guarantees

### Isolated worlds

Runs may begin from the same seed while retaining completely separate mutable state.

### Deterministic initialization

The same compatible manifest and seed reproduce the same initial fictional world.

### Atomic local effects

A mutating tool call commits its state change, mutation event, saved result, and execution progression as one local transaction.

### Idempotent retries

A committed tool call can be retried with the same call ID and request without repeating the side effect. Reusing that call ID with different arguments is rejected.

### Durable execution

Model requests, model responses, tool activity, execution status, and state mutations are persisted so interrupted runs can be inspected and resumed.

### Deterministic chaos

Fault rules and counters are persisted with the run so the same recorded execution can be replayed against the same simulated failure conditions.

### Ground-truth evaluation

Twinwright evaluates persisted world state. It does not trust an agent simply because the agent claims the task succeeded.

### Source-safe replay

Verification replay uses an isolated reconstruction and leaves the source run database unchanged.

### Explicit capability boundaries

Unsupported schemas, routes, behaviors, manifests, chaos rules, and replay conditions fail explicitly instead of silently falling back to fake behavior.

### Runtime authorization

Permissions are enforced in the dispatcher transaction, not through instructions to the model. A denied call runs no service handler or chaos effect, and its decision, result, and ledger event commit together.
