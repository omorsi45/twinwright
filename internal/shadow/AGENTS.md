# Observe-only shadow instructions

Applies to `internal/shadow/` in addition to its ancestors' instructions.

- Shadow mode consumes saved local observations; it never sends network
  requests or replays an external action into a production system.
- Config parsing must reject write-enabled mode, unknown fields, and
  invalid connector types. Every connector must strictly parse its records.
- File sources stay beneath the examples root, including when paths contain
  symlinks. Use root-confined file opening for reads, not lexical checks
  alone. Preserve the 8 MiB cap, including files that grow after stat.
- JSONL means one complete JSON value per line; trailing or concatenated
  values must fail rather than silently disappear.
- Preserve source order and the distinction between an attempted/denied
  action and an action observed to have taken effect.
- Every new connector needs valid fixture controls and adversarial tests.
  Do not claim a connector was tested against a live external system when
  only saved fixtures were used.

Run from the root: `go test -count=1 ./internal/shadow`.
