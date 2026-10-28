#!/usr/bin/env bash
# Run a real model against the billing world, record the provider's HTTP
# exchanges, and show the run can be reconstructed with no model.
#
# Every other test in this repository answers the provider adapters from a
# scripted or recorded source. This is the one place a model that was not written
# for the test makes the decisions, so it is also the only place that can say
# whether a small real model gets through the duplicate-charge scenario and
# whether the recording can reproduce what it did. It needs an
# OpenAI-compatible server (Ollama's /v1 API by default), so it is opt-in:
# nothing in `make verify` or the fast CI lane runs it.
#
# Usage:
#   scripts/live-model.sh [BASE_URL] [MODEL]
#
#   BASE_URL  defaults to http://127.0.0.1:11434/v1
#   MODEL     defaults to $TW_LIVE_MODEL; the script stops with a usage message
#             when neither is set, because "the default model" is not a thing a
#             report can honestly name
#
# Environment:
#   TW_LIVE_OUT               directory for everything the script writes (default:
#                             a fresh mktemp -d, printed at the end)
#   TW_LIVE_PROVIDER_TIMEOUT  per-attempt provider deadline (default 300s). CPU
#                             inference on a shared runner, first call's model
#                             load included, can pass the CLI's 90s default.
#
# What it does:
#   1. builds the CLI and the billing manifest
#   2. runs duplicate-charge with --agent openai-compatible --record-cassette
#      and --steps 12, and prints the evaluation verdict
#   3. replays the run from its ledger (no provider), then from the cassette
#      through the run's own adapter (no server and no key)
#   4. writes live-model-report.json
#
# Files written to $TW_LIVE_OUT: live-model-report.json, live.cassette.jsonl,
# live.db (and its -wal/-shm), billing.manifest.json, the CLI binary, and the
# raw output of each step (run.json, inspect.json, ledger-replay.json,
# cassette-replay.json, *.err). Stale copies of exactly those names are replaced;
# nothing else in the directory is touched. None of them belongs in a commit:
# the cassette and the run output hold a model's prompts and answers.
#
# Exit status is non-zero when recording fails, when the run does not reach
# completed (replay needs a completed run, so reconstruction cannot be shown; the
# report is still written), when ledger replay is not verified, or when cassette
# replay is not verified. The model's evaluation verdict is printed and recorded
# and never gates: a small model failing the scenario is a result, not an error.
#
# The report names the server by host and port only, never the full URL, and
# holds no key or header. model_turns and tool_calls are the ledger summary's
# counts, so they are reported for a run that did not complete too; for such a run
# they count what happened before it stopped. wall_ms is the whole script, the
# build included; run_ms, ledger_replay_ms and cassette_replay_ms are the steps.
# If OPENAI_API_KEY is set the CLI sends it as a bearer token to BASE_URL, as it
# does for any openai-compatible run, and this script fails if that value appears
# in the report, the cassette, the database or the output of a step. A key
# shorter than 8 characters is the exception: the masks ignore one (redact.Secrets),
# so it is not searched for, and the script says so.
set -euo pipefail

BASE_URL="${1:-http://127.0.0.1:11434/v1}"
MODEL="${2:-${TW_LIVE_MODEL:-}}"
STEPS=12

if [ -z "$MODEL" ]; then
  printf 'usage: scripts/live-model.sh [BASE_URL] [MODEL]\n' >&2
  printf 'live-model: no model named. Pass it as the second argument or set TW_LIVE_MODEL.\n' >&2
  exit 2
fi
for tool in go jq curl; do
  command -v "$tool" > /dev/null 2>&1 || { printf 'live-model: %s is required and was not found on PATH\n' "$tool" >&2; exit 2; }
done

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${TW_LIVE_OUT:-$(mktemp -d)}"
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"
BIN="$OUT/twinwright"
MANIFEST="$OUT/billing.manifest.json"
DB="$OUT/live.db"
CASSETTE="$OUT/live.cassette.jsonl"
REPORT="$OUT/live-model-report.json"
cd "$ROOT"

# Only the files this script writes are replaced, so pointing TW_LIVE_OUT at a
# directory that holds anything else does not cost the operator that.
rm -f "$BIN" "$MANIFEST" "$DB" "$DB-wal" "$DB-shm" "$CASSETTE" "$REPORT" \
  "$OUT/run.json" "$OUT/run.err" "$OUT/inspect.json" \
  "$OUT/ledger-replay.json" "$OUT/ledger-replay.err" \
  "$OUT/cassette-replay.json" "$OUT/cassette-replay.err"

step() { printf '\n=== %s\n' "$1"; }
note() { printf 'live-model: %s\n' "$1" >&2; }

# problem records one reason the exit status will be non-zero. They are collected
# rather than acted on so the report is written however far the run got, and
# printed together at the end.
PROBLEMS=""
problem() { PROBLEMS="${PROBLEMS}${1}"$'\n'; }

# now_ms is wall-clock milliseconds. bash 5 has EPOCHREALTIME; otherwise GNU date
# gives nanoseconds; otherwise whole seconds, which makes every timing a multiple
# of 1000 and says so by looking like it.
now_ms() {
  if [ -n "${EPOCHREALTIME:-}" ]; then
    local t="${EPOCHREALTIME/[.,]/}"
    printf '%s' $((t / 1000))
    return
  fi
  local n
  n="$(date +%s%N 2> /dev/null || true)"
  case "$n" in
    *[!0-9]* | '') printf '%s000' "$(date +%s)" ;;
    *) printf '%s' $((n / 1000000)) ;;
  esac
}

# verdict prints true only when the file is a replay report with verified true.
verdict() {
  local v
  v="$(jq -r 'if .verified == true then "true" else "false" end' "$1" 2> /dev/null || true)"
  if [ "$v" = true ]; then printf true; else printf false; fi
}

T_START="$(now_ms)"
STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

step "build the CLI and the billing manifest"
go build -o "$BIN" ./cmd/twinwright
"$BIN" build examples/billing/openapi.yaml \
  --bindings examples/billing/bindings.yaml \
  --out "$MANIFEST" > /dev/null

# The host and port are all this script says about the server, in the report and
# in every message: a base URL can carry userinfo, a path prefix or a query that
# names an account.
HOST="${BASE_URL#*://}"
HOST="${HOST%%[/?#]*}"
HOST="${HOST##*@}"

# The digest is what makes a model name a specific set of weights: a tag can be
# re-pushed. Ollama reports it on /api/tags next to the /v1 API; a server without
# that endpoint, or without the model, leaves it empty rather than failing.
MODEL_DIGEST=""
TAGS_URL="${BASE_URL%/}"
TAGS_URL="${TAGS_URL%/v1}/api/tags"
if TAGS="$(curl -fsS --max-time 10 "$TAGS_URL" 2> /dev/null)"; then
  MODEL_DIGEST="$(printf '%s' "$TAGS" | jq -r --arg m "$MODEL" \
    '[.models[]? | select(.name == $m or .model == $m or .name == ($m + ":latest")) | .digest][0] // ""' 2> /dev/null || true)"
fi
[ -n "$MODEL_DIGEST" ] || note "no model digest from $HOST (/api/tags); model_digest will be empty"

step "run duplicate-charge with $MODEL (steps $STEPS, recording $CASSETTE)"
T0="$(now_ms)"
set +e
"$BIN" run duplicate-charge \
  --manifest "$MANIFEST" --db "$DB" \
  --agent openai-compatible --model "$MODEL" --base-url "$BASE_URL" \
  --provider-timeout "${TW_LIVE_PROVIDER_TIMEOUT:-300s}" \
  --steps "$STEPS" --record-cassette "$CASSETTE" \
  > "$OUT/run.json" 2> "$OUT/run.err"
RUN_RC=$?
set -e
RUN_MS=$(($(now_ms) - T0))

# A run that failed prints no result, only an error naming the run, so the ID
# comes from the result when there is one and from the error otherwise.
RUN_ID="$(jq -r '.run.id // empty' "$OUT/run.json" 2> /dev/null || true)"
[ -n "$RUN_ID" ] || RUN_ID="$(grep -o 'R-[0-9a-f]\{24\}' "$OUT/run.err" | head -n 1 || true)"

EVAL_PASSED=null
RUN_STATUS=""
MODEL_TURNS=null
TOOL_CALLS=null
if [ "$RUN_RC" -ne 0 ]; then
  problem "the run command exited $RUN_RC"
  head -c 2000 "$OUT/run.err" >&2 || true
fi
if [ -z "$RUN_ID" ]; then
  problem "recording failed: the run left no run ID"
else
  set +e
  "$BIN" inspect "$RUN_ID" --manifest "$MANIFEST" --db "$DB" > "$OUT/inspect.json" 2> /dev/null
  set -e
  RUN_STATUS="$(jq -r '.run.status // empty' "$OUT/inspect.json" 2> /dev/null || true)"
  MODEL_TURNS="$(jq -c '.summary.model_turns // null' "$OUT/inspect.json" 2> /dev/null || echo null)"
  TOOL_CALLS="$(jq -c '.summary.tool_calls // null' "$OUT/inspect.json" 2> /dev/null || echo null)"
  [ -n "$MODEL_TURNS" ] || MODEL_TURNS=null
  [ -n "$TOOL_CALLS" ] || TOOL_CALLS=null
fi
if [ "$RUN_RC" -eq 0 ]; then
  EVAL_PASSED="$(jq -c '.evaluation.passed' "$OUT/run.json" 2> /dev/null || echo null)"
  [ -n "$EVAL_PASSED" ] || EVAL_PASSED=null
fi

# The cassette is the recording. A header with no exchange behind it, or no
# header at all, means nothing was recorded however the run ended.
CASSETTE_LINES=0
if [ -f "$CASSETTE" ]; then CASSETTE_LINES=$(($(wc -l < "$CASSETTE"))); fi
EXCHANGES=0
if [ "$CASSETTE_LINES" -gt 0 ]; then EXCHANGES=$((CASSETTE_LINES - 1)); fi
if [ ! -f "$CASSETTE" ] || ! head -n 1 "$CASSETTE" | jq -e '.twinwright_cassette == 1' > /dev/null 2>&1; then
  problem "recording failed: $CASSETTE has no cassette header"
elif [ "$EXCHANGES" -eq 0 ]; then
  problem "recording failed: the cassette holds no exchange"
fi
if grep -q 'write cassette' "$OUT/run.err" 2> /dev/null; then
  problem "recording failed: the cassette could not be written"
fi

printf '\nrun %s ended %s after %s model turns and %s tool calls\n' "${RUN_ID:-none}" "${RUN_STATUS:-unknown}" "$MODEL_TURNS" "$TOOL_CALLS"
printf 'evaluation passed: %s\n' "$EVAL_PASSED"
printf 'cassette exchanges: %s\n' "$EXCHANGES"

LEDGER_VERIFIED=false
CASSETTE_VERIFIED=false
LEDGER_MS=0
CASSETTE_MS=0
if [ "$RUN_STATUS" = completed ]; then
  step "replay from the ledger (no provider)"
  T0="$(now_ms)"
  set +e
  "$BIN" replay "$RUN_ID" --manifest "$MANIFEST" --db "$DB" > "$OUT/ledger-replay.json" 2> "$OUT/ledger-replay.err"
  set -e
  LEDGER_MS=$(($(now_ms) - T0))
  LEDGER_VERIFIED="$(verdict "$OUT/ledger-replay.json")"
  cat "$OUT/ledger-replay.json"
  [ "$LEDGER_VERIFIED" = true ] || problem "ledger replay was not verified: $(head -c 500 "$OUT/ledger-replay.err" 2> /dev/null || true)"

  # Without the key or a base URL in the environment, so a verdict here cannot
  # have come from either: the adapter is rebuilt from the run record and the
  # cassette alone.
  step "replay from the cassette through the run's own adapter (no server, no key)"
  T0="$(now_ms)"
  set +e
  env -u OPENAI_API_KEY -u OPENAI_BASE_URL "$BIN" replay "$RUN_ID" --manifest "$MANIFEST" --db "$DB" \
    --cassette "$CASSETTE" > "$OUT/cassette-replay.json" 2> "$OUT/cassette-replay.err"
  set -e
  CASSETTE_MS=$(($(now_ms) - T0))
  CASSETTE_VERIFIED="$(verdict "$OUT/cassette-replay.json")"
  cat "$OUT/cassette-replay.json"
  [ "$CASSETTE_VERIFIED" = true ] || problem "cassette replay was not verified: $(head -c 500 "$OUT/cassette-replay.err" 2> /dev/null || true)"
elif [ -n "$RUN_ID" ]; then
  problem "the run ended ${RUN_STATUS:-in an unknown state}, not completed, and replay needs a completed run, so neither replay was attempted"
fi

WALL_MS=$(($(now_ms) - T_START))
jq -n \
  --arg model "$MODEL" \
  --arg host "$HOST" \
  --arg digest "$MODEL_DIGEST" \
  --arg run_id "$RUN_ID" \
  --arg run_status "$RUN_STATUS" \
  --arg machine "$(uname -sm)" \
  --arg go_version "$(go env GOVERSION)" \
  --arg git_revision "$(git rev-parse HEAD 2> /dev/null || true)" \
  --arg started_at "$STARTED_AT" \
  --argjson evaluation_passed "$EVAL_PASSED" \
  --argjson model_turns "$MODEL_TURNS" \
  --argjson tool_calls "$TOOL_CALLS" \
  --argjson ledger_verified "$LEDGER_VERIFIED" \
  --argjson cassette_verified "$CASSETTE_VERIFIED" \
  --argjson exchanges "$EXCHANGES" \
  --argjson run_ms "$RUN_MS" \
  --argjson ledger_ms "$LEDGER_MS" \
  --argjson cassette_ms "$CASSETTE_MS" \
  --argjson wall_ms "$WALL_MS" \
  '{
    model: $model,
    base_url_host: $host,
    evaluation_passed: $evaluation_passed,
    model_turns: $model_turns,
    tool_calls: $tool_calls,
    ledger_replay_verified: $ledger_verified,
    cassette_replay_verified: $cassette_verified,
    cassette_exchanges: $exchanges,
    wall_ms: $wall_ms,
    run_id: $run_id,
    run_status: $run_status,
    model_digest: $digest,
    machine: $machine,
    go_version: $go_version,
    git_revision: $git_revision,
    started_at: $started_at,
    run_ms: $run_ms,
    ledger_replay_ms: $ledger_ms,
    cassette_replay_ms: $cassette_ms
  }' > "$REPORT"

# No secret may reach anything written here. The recorder masks the key in the
# cassette and the CLI masks it in errors; this is the check that says so about
# this run's files rather than about the code in general. grep reads the key from
# a pipe so it never appears in a process listing.
#
# A key shorter than 8 characters is not checked. The masks ignore one on purpose
# (redact.Secrets, so that ordinary words are never rewritten), so this check
# cannot ask for more than the masks give: Ollama's own documentation uses the
# placeholder key "ollama", which is in every response as "fp_ollama" and would
# fail every clean run. The check says so when it skips.
#
# The files are the report, the cassette, the database and its write-ahead log,
# and the output of each step. The CLI binary and the manifest are not searched.
if [ -n "${OPENAI_API_KEY:-}" ]; then
  if [ "${#OPENAI_API_KEY}" -lt 8 ]; then
    note "OPENAI_API_KEY is shorter than 8 characters, which the masks ignore by design, so it is not searched for in the output files"
  else
    for f in "$REPORT" "$CASSETTE" "$DB" "$DB-wal" "$OUT"/run.json "$OUT"/run.err "$OUT"/inspect.json \
      "$OUT"/ledger-replay.json "$OUT"/ledger-replay.err "$OUT"/cassette-replay.json "$OUT"/cassette-replay.err; do
      [ -f "$f" ] || continue
      if grep -qaF -f <(printf '%s\n' "$OPENAI_API_KEY") "$f"; then
        problem "the value of OPENAI_API_KEY appears in $(basename "$f")"
      fi
    done
  fi
fi

step "report"
cat "$REPORT"
printf '\nlive-model: outputs are in %s\n' "$OUT"

if [ -n "$PROBLEMS" ]; then
  printf '\nlive-model: failed:\n%s' "$PROBLEMS" >&2
  exit 1
fi
printf 'live-model: recorded, and both replays verified (evaluation passed: %s)\n' "$EVAL_PASSED"
