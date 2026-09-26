#!/usr/bin/env bash
# Deterministic end-to-end walk of Twinwright's documented pipeline.
#
# Every step uses scripted fixtures, so this needs no API key, no network and no
# container runtime, and produces the same result on every run. CI runs it to
# keep the README's commands from drifting away from the binary, and it doubles
# as the fastest way for a new contributor to see the whole system work.
#
# Usage:
#   scripts/e2e.sh                 # SQLite in a temporary directory
#   scripts/e2e.sh "postgres://…"  # against a PostgreSQL server
set -euo pipefail

DSN_BASE="${1:-}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="$(mktemp -d)"
BIN="$WORK/twinwright"
trap 'rm -rf "$WORK"' EXIT

cd "$ROOT"

step() { printf '\n=== %s\n' "$1"; }
fail() { printf 'e2e: %s\n' "$1" >&2; exit 1; }

# db <name> returns a storage target. With a PostgreSQL DSN each logical
# database becomes its own schema, so the steps stay as isolated as the
# separate SQLite files they replace.
db() {
  if [ -n "$DSN_BASE" ]; then
    case "$DSN_BASE" in
      *\?*) printf '%s&search_path=e2e_%s' "$DSN_BASE" "$1" ;;
      *)    printf '%s?search_path=e2e_%s' "$DSN_BASE" "$1" ;;
    esac
  else
    printf '%s/%s.db' "$WORK" "$1"
  fi
}

# run_id extracts the first run identifier from a command's JSON output.
run_id() { grep -o 'R-[0-9a-f]\{24\}' | head -n 1; }

step "build the CLI"
go build -o "$BIN" ./cmd/twinwright
"$BIN" version

step "doctor"
"$BIN" doctor --db "$(db doctor)" > "$WORK/doctor.json"
grep -q '"healthy": true' "$WORK/doctor.json" || { cat "$WORK/doctor.json"; fail "doctor reported an unhealthy environment"; }

step "compile the billing service and the multi-service company world"
"$BIN" build examples/billing/openapi.yaml \
  --bindings examples/billing/bindings.yaml \
  --out "$WORK/billing.manifest.json"
"$BIN" build-world examples/company/world.yaml --out "$WORK/company.manifest.json"

step "run the duplicate-charge scenario"
BILLING_DB="$(db billing)"
RUN_ID="$("$BIN" run duplicate-charge \
  --manifest "$WORK/billing.manifest.json" --db "$BILLING_DB" \
  --agent scripted --seed 42 | tee "$WORK/run.json" | run_id)"
[ -n "$RUN_ID" ] || { cat "$WORK/run.json"; fail "could not read a run ID"; }
grep -q '"passed":true' "$WORK/run.json" || { cat "$WORK/run.json"; fail "duplicate-charge evaluation did not pass"; }
printf 'run %s\n' "$RUN_ID"

step "inspect and trace"
"$BIN" inspect "$RUN_ID" --manifest "$WORK/billing.manifest.json" --db "$BILLING_DB" > /dev/null
"$BIN" trace "$RUN_ID" --db "$BILLING_DB" > "$WORK/trace.json"
"$BIN" trace "$RUN_ID" --format text --db "$BILLING_DB" > "$WORK/trace.txt"
"$BIN" trace "$RUN_ID" --format otlp --db "$BILLING_DB" > "$WORK/trace.otlp.json"
grep -q 'resourceSpans' "$WORK/trace.otlp.json" || fail "OTLP trace output is missing resourceSpans"

step "declarative assertions"
"$BIN" evaluate "$RUN_ID" \
  --assertions examples/assertions/duplicate-charge.yaml \
  --manifest "$WORK/billing.manifest.json" --db "$BILLING_DB" > "$WORK/assertions.json"
grep -q '"passed":true' "$WORK/assertions.json" || { cat "$WORK/assertions.json"; fail "assertions did not pass"; }

step "replay verification"
"$BIN" replay "$RUN_ID" --manifest "$WORK/billing.manifest.json" --db "$BILLING_DB" > "$WORK/replay.json"
grep -q '"verified":true' "$WORK/replay.json" || { cat "$WORK/replay.json"; fail "replay did not verify" ; }

step "checkpoints, fork and compare"
"$BIN" checkpoints "$RUN_ID" --manifest "$WORK/billing.manifest.json" --db "$BILLING_DB" > "$WORK/checkpoints.json"
FORK_SEQ="$(grep -o '"event_seq":[0-9]*' "$WORK/checkpoints.json" | head -n 1 | cut -d: -f2)"
[ -n "$FORK_SEQ" ] || { cat "$WORK/checkpoints.json"; fail "no checkpoint to fork from"; }
CHILD_ID="$("$BIN" fork "$RUN_ID" --at-event "$FORK_SEQ" \
  --manifest "$WORK/billing.manifest.json" --db "$BILLING_DB" | tee "$WORK/fork.json" | run_id)"
[ -n "$CHILD_ID" ] || { cat "$WORK/fork.json"; fail "fork produced no child run"; }
[ "$CHILD_ID" != "$RUN_ID" ] || fail "fork returned the parent run ID"
"$BIN" compare "$RUN_ID" "$CHILD_ID" --db "$BILLING_DB" > "$WORK/compare.json"

step "chaos: the ambiguous commit, recovered safely and unsafely"
SAFE_DB="$(db safe)"
"$BIN" run ambiguous-commit --manifest "$WORK/billing.manifest.json" --db "$SAFE_DB" \
  --agent scripted --recovery safe --chaos examples/chaos/ambiguous-commit.yaml > "$WORK/safe.json"
grep -q '"passed":true' "$WORK/safe.json" || { cat "$WORK/safe.json"; fail "safe recovery did not pass evaluation"; }

UNSAFE_DB="$(db unsafe)"
# The unsafe fixture is supposed to fail evaluation: it retries a committed
# refund with a new call ID and pays twice. A passing run here would mean the
# chaos engine stopped injecting the ambiguity.
set +e
UNSAFE_ID="$("$BIN" run ambiguous-commit --manifest "$WORK/billing.manifest.json" --db "$UNSAFE_DB" \
  --agent scripted --recovery unsafe --chaos examples/chaos/ambiguous-commit.yaml 2>"$WORK/unsafe.err" \
  | tee "$WORK/unsafe.json" | run_id)"
set -e
[ -n "$UNSAFE_ID" ] || { cat "$WORK/unsafe.json" "$WORK/unsafe.err"; fail "unsafe run produced no run ID"; }
grep -q '"unsafe_retry":{"detected":true' "$WORK/unsafe.json" || { cat "$WORK/unsafe.json"; fail "the unsafe retry was not detected"; }

step "counterfactual analysis of the unsafe run"
"$BIN" counterfactual "$UNSAFE_ID" \
  --interventions examples/counterfactual/ambiguous-commit.yaml \
  --trials 1 --manifest "$WORK/billing.manifest.json" --db "$UNSAFE_DB" > "$WORK/counterfactual.json"
grep -q '"candidates"' "$WORK/counterfactual.json" || { cat "$WORK/counterfactual.json"; fail "counterfactual produced no candidates"; }

step "benchmark smoke"
"$BIN" bench --suite standard --agent scripted --examples examples --out "$WORK/bench.json" > "$WORK/bench.stdout"
grep -q '"cases"' "$WORK/bench.json" || { head -c 2000 "$WORK/bench.json"; fail "benchmark report has no cases"; }

step "distributed: enqueue, claim under a fenced lease, finish, replay"
DIST_DB="$(db distributed)"
DIST_RUN="$("$BIN" run duplicate-charge \
  --manifest "$WORK/billing.manifest.json" --db "$DIST_DB" \
  --agent scripted --enqueue --steps 10 | tee "$WORK/enqueue.json" | run_id)"
[ -n "$DIST_RUN" ] || { cat "$WORK/enqueue.json"; fail "enqueue produced no run ID"; }
grep -q '"delivery": "at_least_once"' "$WORK/enqueue.json" || fail "enqueue must state at-least-once delivery"

"$BIN" queue --db "$DIST_DB" > "$WORK/queue-before.json"
grep -q '"runnable": 1' "$WORK/queue-before.json" || { cat "$WORK/queue-before.json"; fail "the run was not queued"; }

"$BIN" worker --manifest "$WORK/billing.manifest.json" --db "$DIST_DB" \
  --id e2e-worker-1 --drain --lease-ttl 60s > "$WORK/worker.json" 2>"$WORK/worker.log"
grep -q '"disposition": "finished"' "$WORK/worker.json" || { cat "$WORK/worker.json" "$WORK/worker.log"; fail "the worker did not finish the run"; }
grep -q '"run_status": "completed"' "$WORK/worker.json" || { cat "$WORK/worker.json"; fail "the claimed run did not complete"; }

"$BIN" queue --db "$DIST_DB" --run "$DIST_RUN" > "$WORK/ownership.json"
grep -q 'lease.acquired' "$WORK/ownership.json" || { cat "$WORK/ownership.json"; fail "no lease acquisition was recorded"; }
grep -q 'run.finished' "$WORK/ownership.json" || { cat "$WORK/ownership.json"; fail "the run completion was not recorded in the ownership log"; }

# A distributed run must be as auditable as a local one.
"$BIN" replay "$DIST_RUN" --manifest "$WORK/billing.manifest.json" --db "$DIST_DB" > "$WORK/dist-replay.json"
grep -q '"verified":true' "$WORK/dist-replay.json" || { cat "$WORK/dist-replay.json"; fail "the distributed run did not replay"; }

# A second drain must find nothing: a finished run is not re-delivered.
"$BIN" worker --manifest "$WORK/billing.manifest.json" --db "$DIST_DB" \
  --id e2e-worker-2 --drain > "$WORK/worker2.json" 2>/dev/null
grep -q '"claims": 0' "$WORK/worker2.json" || { cat "$WORK/worker2.json"; fail "a finished run was re-delivered"; }

step "shadow mode (observe only)"
"$BIN" shadow --config examples/shadow/observe-only.yaml --examples examples \
  --manifest "$WORK/billing.manifest.json" --scenario duplicate-charge --agent scripted > "$WORK/shadow.json"
grep -q '"comparison"' "$WORK/shadow.json" || { cat "$WORK/shadow.json"; fail "shadow produced no comparison"; }

printf '\ne2e: every step passed\n'
