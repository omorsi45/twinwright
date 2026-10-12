# Agent runner and provider instructions

Applies to `internal/agent/` in addition to its ancestors' instructions.

- `runner.go` owns durable turns and resumable execution. Keep stored
  transcript, tool requests, model invocation events, and replay aligned.
- Preserve the exact request and call IDs across an interrupted provider
  attempt. A truncated model output is not a valid assistant decision.
- Retry only interruptions according to the existing policy. Record attempts
  so replay works without a second live provider call.
- Provider adapters must redact credentials and keep the default test suite
  independent of external APIs.
- For a provider change, test malformed output, interruption or cancellation,
  replayability, and affected transcript/ledger behavior.

Run from the root: `go test -count=1 ./internal/agent`.
