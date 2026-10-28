#!/usr/bin/env bash
# One-command demo of the platform around the runtime: a PostgreSQL-backed queue,
# a worker killed while it holds a run, a second worker taking that run over, and
# the OpenTelemetry Collector, Jaeger and Prometheus showing what happened. Every
# claim it prints is read back from one of those systems, and the exit status is
# non-zero when any of the six checks at the end is missing.
#
# Usage:
#   scripts/demo-stack.sh        # or: make demo-stack
#
# Needs bash, curl, jq, go and docker compose. It starts the Compose services
# (postgres, otel-collector, jaeger, prometheus) and LEAVES THEM RUNNING, so the
# Jaeger and Prometheus UIs can be opened afterwards; `make down` stops them and
# removes their data.
#
# Environment:
#   TW_DEMO_LOGS  when set, the worker logs and outcome files are copied to this
#                 directory on exit, with any password in a DSN masked. CI sets it
#                 so a failed run leaves something to read.
#
# What it does to the PostgreSQL server:
#   It never touches the `twinwright` database, which is the developer's. It drops
#   and recreates two databases on every run, twinwright_demo_company (6
#   company-incident runs) and twinwright_demo_billing (2 duplicate-charge runs).
#   A worker serves exactly one manifest and the company world refuses the
#   duplicate-charge scenario, so the two workloads need two databases and two
#   manifests. Only the database names are printed, never a DSN.
#
# The workers, all on the host (Compose runs no worker image):
#   demo-worker-a        company database, metrics on 0.0.0.0:9095, killed with
#                        SIGKILL while it holds a run
#   demo-worker-b        company database, metrics on 0.0.0.0:9096, takes the run
#                        over once the lease lapses
#   demo-worker-billing  billing database, no metrics, drains and exits
# All three export each completed run's trace to the collector. The metrics
# listeners bind every interface because the Prometheus container reaches the host
# through the bridge gateway, which a loopback listener does not answer; they have
# no authentication and exist only while the demo runs.
#
# Why worker B is not started with --drain: when A dies holding a lease, the only
# entry left is leased until the TTL lapses, so a draining B would see no claimable
# work, exit before the takeover, and stop serving metrics before Prometheus
# scraped it. B runs in serve mode and the script stops it with SIGINT at the end,
# which releases cleanly.
#
# How the kill lands while A holds the run: a scripted run finishes in
# milliseconds, so "kill A after its first claim" would otherwise be a race. A
# separate PostgreSQL session takes LOCK TABLE events IN SHARE MODE on the company
# database before A starts. A claims a run (the claim touches work_queue,
# run_leases, run_ownership_log and workers, never events), then blocks on its
# first insert into events, the ledger table. The script waits until the queue
# shows the lease held by A, waits for Prometheus to scrape A once, kills A with
# SIGKILL, ends the lock session with pg_terminate_backend, and only then starts B.
# SHARE mode is deliberate: it blocks inserts but not reads, and every scrape of a
# worker's metrics endpoint runs a SELECT on events, so ACCESS EXCLUSIVE would stop
# Prometheus from ever scraping A while it holds the run.
#
# The checks (numbered in the summary):
#   1. all 8 runs completed
#   2. the ownership log shows A's lease taken over by B
#   3. Prometheus counts the takeover (twinwright_worker_takeovers_total, worker B)
#   4. Prometheus scraped both workers (A's target is down after its kill, which is
#      expected)
#   5. every run's trace is queryable in Jaeger by its trace ID
#   6. the benchmark gate passes against the approved baseline (SQLite, temp dir)
#
# Delivery stays at-least-once: the takeover re-executes the run from the ledger,
# and nothing here adds to the ledger beyond normal runs.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

COMPANY_DB="twinwright_demo_company"
BILLING_DB="twinwright_demo_billing"
LOCK_APP="twinwright-demo-lock"
WORKER_A="demo-worker-a"
WORKER_B="demo-worker-b"
WORKER_BILLING="demo-worker-billing"
A_PORT=9095
B_PORT=9096
OTLP_ENDPOINT="http://127.0.0.1:4318"
COLLECTOR_HEALTH="http://127.0.0.1:13133/"
JAEGER_URL="http://127.0.0.1:16686"
PROM_URL="http://127.0.0.1:9090"
PROM_JOB="twinwright-workers"

CURRENT_STEP="start"
step() { CURRENT_STEP="$1"; printf '\n=== %s\n' "$1"; }
note() { printf 'demo-stack: %s\n' "$1"; }

for tool in curl jq go docker; do
  command -v "$tool" > /dev/null 2>&1 || { printf 'demo-stack: %s is required and was not found on PATH\n' "$tool" >&2; exit 2; }
done
docker compose version > /dev/null 2>&1 || { printf 'demo-stack: docker compose is required and was not found\n' >&2; exit 2; }

WORK="$(mktemp -d)"
BIN="$WORK/twinwright"
COMPANY_MANIFEST="$WORK/company.manifest.json"
BILLING_MANIFEST="$WORK/billing.manifest.json"

# scrub masks the password of any PostgreSQL URL on its way to a terminal or a
# file that leaves the work directory. The DSNs are built from the Compose file's
# development credentials and are never printed on purpose; this covers an error
# message that quotes one.
scrub() { sed -E 's#(postgres(ql)?://)[^@/[:space:]]*@#\1***@#g'; }

dsn() { printf 'postgres://twinwright:twinwright@127.0.0.1:5432/%s?sslmode=disable' "$1"; }

A_PID=""
B_PID=""
C_PID=""
LOCK_PID=""

# alive is true for a running process. An empty pid is not running. A child that
# has ended is reaped by the shell at once, so it does not linger as a zombie that
# kill -0 would still accept.
alive() { [ -n "${1:-}" ] && kill -0 "$1" 2> /dev/null; }
exited() { ! alive "$1"; }

# stop_hard ends a worker the script started: SIGTERM, a short wait, then SIGKILL.
stop_hard() {
  alive "${1:-}" || return 0
  kill "$1" 2> /dev/null || true
  local i
  for i in 1 2 3 4 5 6 7 8 9 10; do
    alive "$1" || break
    sleep 0.5
  done
  alive "$1" && kill -9 "$1" 2> /dev/null
  { wait "$1"; } 2> /dev/null || true
  return 0
}

# end_lock_session ends the lock session on the server and then its client. It is
# safe to call twice and when the stack is down.
end_lock_session() {
  docker compose exec -T postgres psql -U twinwright -d postgres -Atq \
    -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = '$LOCK_APP'" \
    > /dev/null 2>&1 || true
  if alive "$LOCK_PID"; then
    kill "$LOCK_PID" 2> /dev/null || true
    { wait "$LOCK_PID"; } 2> /dev/null || true
  fi
  LOCK_PID=""
  return 0
}

save_logs() {
  [ -n "${TW_DEMO_LOGS:-}" ] || return 0
  mkdir -p "$TW_DEMO_LOGS" || return 0
  local f
  for f in "$WORK"/worker-*.log "$WORK"/worker-*.json "$WORK"/lock-session.log \
    "$WORK"/held-run-queue.json "$WORK"/run-statuses.txt "$WORK"/gate.out; do
    [ -f "$f" ] || continue
    scrub < "$f" > "$TW_DEMO_LOGS/$(basename "$f")" || true
  done
  return 0
}

# cleanup runs on every exit. A worker the script started never outlives it, and
# the lock session always ends, so a failed run cannot leave the company database
# frozen for the next one (the next run also drops the database with FORCE).
cleanup() {
  local rc=$?
  trap - EXIT
  set +e
  stop_hard "$A_PID"
  stop_hard "$B_PID"
  stop_hard "$C_PID"
  end_lock_session
  save_logs
  if [ "$rc" -ne 0 ]; then
    printf '\ndemo-stack: exited %s. Work directory: %s\n' "$rc" "$WORK" >&2
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# fail names the step that was running, prints the tail of the logs that might
# explain it, and ends the script (the EXIT trap does the cleanup).
fail() {
  printf '\ndemo-stack: FAILED during "%s": %s\n' "$CURRENT_STEP" "$1" >&2
  local f
  for f in "$WORK"/worker-*.log "$WORK"/lock-session.log; do
    [ -s "$f" ] || continue
    printf -- '--- tail of %s\n' "$(basename "$f")" >&2
    tail -n 15 "$f" | scrub >&2
  done
  exit 1
}

# wait_until runs a command every interval until it succeeds or the limit passes.
# It returns 1 on timeout so a caller can decide whether that is fatal. Every wait
# in this script goes through it, so none is unbounded.
wait_until() {
  local limit="$1" interval="$2"
  shift 2
  local deadline=$(($(date +%s) + limit))
  while :; do
    if "$@"; then return 0; fi
    [ "$(date +%s)" -lt "$deadline" ] || return 1
    sleep "$interval"
  done
}

# require_wait is wait_until that ends the script, naming what it waited for.
require_wait() {
  local what="$1" limit="$2"
  shift 2
  wait_until "$limit" 1 "$@" || fail "timed out after ${limit}s waiting for $what"
}

# psql_admin talks to the maintenance database, so databases can be dropped and
# recreated. -T because there is no terminal in CI.
psql_admin() { docker compose exec -T postgres psql -U twinwright -d postgres -v ON_ERROR_STOP=1 "$@"; }

http_ok() { curl -fsS --max-time 3 "$1" > /dev/null 2>&1; }

# pg_ready asks over TCP. The image's first-boot server listens on a Unix socket
# only and then restarts, so a pg_isready over the socket can answer before the
# real server is up.
pg_ready() {
  docker compose exec -T postgres pg_isready -h 127.0.0.1 -U twinwright > /dev/null 2>&1 &&
    psql_admin -Atq -c 'SELECT 1' > /dev/null 2>&1
}

# prom_value prints the largest sample value an instant query returns, or nothing
# when it returns no sample or Prometheus does not answer.
prom_value() {
  curl -fsS --max-time 5 -G --data-urlencode "query=$1" "$PROM_URL/api/v1/query" 2> /dev/null |
    jq -r '[.data.result[]?.value[1] | tonumber] | max // empty' 2> /dev/null || true
}

# prom_at_least succeeds when the query's value is a number >= the minimum.
prom_at_least() {
  local value
  value="$(prom_value "$1")"
  [ -n "$value" ] && jq -en --argjson v "$value" --argjson m "$2" '$v >= $m' > /dev/null 2>&1
}

# EVIDENCE collects the numbered checks. They are recorded, not acted on, so a
# missing one does not hide the others: the summary shows all six and the exit
# status carries the verdict.
EVIDENCE_COUNT=0
EVIDENCE_FAILED=0
EVIDENCE=""
evidence() {
  local ok="$1" title="$2" detail="$3" mark="PASS" line
  EVIDENCE_COUNT=$((EVIDENCE_COUNT + 1))
  if [ "$ok" -ne 0 ]; then
    mark="FAIL"
    EVIDENCE_FAILED=$((EVIDENCE_FAILED + 1))
  fi
  line="$(printf '%d. [%s] %s: %s' "$EVIDENCE_COUNT" "$mark" "$title" "$detail")"
  EVIDENCE="${EVIDENCE}${line}"$'\n'
  printf '%s\n' "$line"
}

printf 'demo-stack: work directory %s (binary, manifests, worker logs and outcomes)\n' "$WORK"

# --------------------------------------------------------------------------
step "start PostgreSQL, the OpenTelemetry Collector, Jaeger and Prometheus"
docker compose up -d postgres otel-collector jaeger prometheus || fail "docker compose up failed"

require_wait "PostgreSQL to accept connections (pg_isready over TCP, then SELECT 1)" 90 pg_ready
require_wait "the collector's health endpoint $COLLECTOR_HEALTH" 90 http_ok "$COLLECTOR_HEALTH"
require_wait "the Jaeger UI at $JAEGER_URL/" 90 http_ok "$JAEGER_URL/"
require_wait "Prometheus to be ready at $PROM_URL/-/ready" 90 http_ok "$PROM_URL/-/ready"

# A Prometheus left running by an earlier checkout still has the one-target
# configuration, and the bind-mounted file is not re-read until it restarts.
prom_has_both_targets() {
  curl -fsS --max-time 5 "$PROM_URL/api/v1/status/config" 2> /dev/null |
    jq -e --arg a "host.docker.internal:$A_PORT" --arg b "host.docker.internal:$B_PORT" \
      '.data.yaml | (contains($a) and contains($b))' > /dev/null 2>&1
}
if ! prom_has_both_targets; then
  note "the running Prometheus does not scrape both worker ports; restarting it to load deploy/prometheus.yml"
  docker compose restart prometheus > /dev/null || fail "docker compose restart prometheus failed"
  require_wait "Prometheus to be ready after the restart" 90 http_ok "$PROM_URL/-/ready"
  prom_has_both_targets || fail "Prometheus does not scrape both worker ports after the restart; check deploy/prometheus.yml"
fi
note "the stack is up"

# --------------------------------------------------------------------------
step "build the CLI and the two manifests"
go build -o "$BIN" ./cmd/twinwright || fail "go build failed"
"$BIN" version
"$BIN" build-world examples/company/world.yaml --out "$COMPANY_MANIFEST" > /dev/null ||
  fail "could not compile the company world"
"$BIN" build examples/billing/openapi.yaml --bindings examples/billing/bindings.yaml \
  --out "$BILLING_MANIFEST" > /dev/null || fail "could not compile the billing service"

# --------------------------------------------------------------------------
step "recreate the demo databases (the twinwright database is not touched)"
for database in "$COMPANY_DB" "$BILLING_DB"; do
  psql_admin -q -c "DROP DATABASE IF EXISTS $database WITH (FORCE)" -c "CREATE DATABASE $database" ||
    fail "could not recreate database $database"
  note "database $database is empty"
done

# --------------------------------------------------------------------------
step "enqueue 6 company-incident runs and 2 duplicate-charge runs"
# enqueue prints the new run's ID. The run is created and queued, not executed.
enqueue() {
  local scenario="$1" manifest="$2" database="$3" steps="$4" id
  id="$("$BIN" run "$scenario" --manifest "$manifest" --db "$(dsn "$database")" \
    --agent scripted --seed 42 --enqueue --steps "$steps" 2> "$WORK/enqueue.err" | jq -r '.run_id // empty')" ||
    fail "could not enqueue a $scenario run on $database: $(scrub < "$WORK/enqueue.err")"
  [[ "$id" =~ ^R-[0-9a-f]{24}$ ]] || fail "enqueueing a $scenario run on $database returned no run ID"
  printf '%s' "$id"
}
# RUN_TABLE lists every run as "<id> <database> <manifest>", one per line, for the
# checks that visit all eight.
COMPANY_RUNS=""
RUN_TABLE=""
for _ in 1 2 3 4 5 6; do
  id="$(enqueue company-incident "$COMPANY_MANIFEST" "$COMPANY_DB" 20)"
  COMPANY_RUNS="$COMPANY_RUNS $id"
  RUN_TABLE="${RUN_TABLE}${id} ${COMPANY_DB} ${COMPANY_MANIFEST}"$'\n'
  note "queued $id (company-incident) on $COMPANY_DB"
done
for _ in 1 2; do
  id="$(enqueue duplicate-charge "$BILLING_MANIFEST" "$BILLING_DB" 10)"
  RUN_TABLE="${RUN_TABLE}${id} ${BILLING_DB} ${BILLING_MANIFEST}"$'\n'
  note "queued $id (duplicate-charge) on $BILLING_DB"
done

queue_company() { "$BIN" queue --db "$(dsn "$COMPANY_DB")" "$@"; }

# --------------------------------------------------------------------------
step "worker $WORKER_BILLING drains the billing database"
"$BIN" worker --manifest "$BILLING_MANIFEST" --db "$(dsn "$BILLING_DB")" --id "$WORKER_BILLING" \
  --lease-ttl 30s --poll 200ms --otlp-endpoint "$OTLP_ENDPOINT" --drain \
  > "$WORK/worker-billing.json" 2> "$WORK/worker-billing.log" &
C_PID=$!
wait_until 120 1 exited "$C_PID" ||
  fail "timed out after 120s waiting for $WORKER_BILLING to drain the billing database"
C_RC=0
{ wait "$C_PID"; } 2> /dev/null || C_RC=$?
C_PID=""
[ "$C_RC" -eq 0 ] || fail "$WORKER_BILLING exited $C_RC"
note "$WORKER_BILLING drained the billing database and exited"

# --------------------------------------------------------------------------
step "hold the company database's ledger so worker A cannot finish the run it claims"
# SET application_name lets the script find this session again to end it. The
# lock is taken inside BEGIN and kept by pg_sleep, so it lasts until the session is
# ended or two minutes pass, whichever is first.
{
  printf '%s\n' "SET application_name = '$LOCK_APP';" "BEGIN;" \
    "LOCK TABLE events IN SHARE MODE;" "SELECT pg_sleep(120);"
} | docker compose exec -T postgres psql -U twinwright -d "$COMPANY_DB" -v ON_ERROR_STOP=1 -q \
  > "$WORK/lock-session.log" 2>&1 &
LOCK_PID=$!

lock_held() {
  local held
  held="$(psql_admin -Atq -c "SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a USING (pid) WHERE a.application_name = '$LOCK_APP' AND l.locktype = 'relation' AND l.mode = 'ShareLock' AND l.granted" 2> /dev/null)" || return 1
  [ "$held" -ge 1 ] 2> /dev/null
}
require_wait "the lock session to hold a SHARE lock on the events table" 30 lock_held
note "the events table is locked against inserts (reads still work)"

# --------------------------------------------------------------------------
step "start worker A alone and wait for it to hold a lease"
note "the metrics endpoints of workers A and B (ports $A_PORT and $B_PORT) are unauthenticated and listen on all interfaces while the demo runs"
T_WORKERS="$(date +%s)"
"$BIN" worker --manifest "$COMPANY_MANIFEST" --db "$(dsn "$COMPANY_DB")" --id "$WORKER_A" \
  --lease-ttl 5s --poll 200ms --metrics-addr "0.0.0.0:$A_PORT" --otlp-endpoint "$OTLP_ENDPOINT" --drain \
  > "$WORK/worker-a.json" 2> "$WORK/worker-a.log" &
A_PID=$!

a_holds_a_lease() {
  alive "$A_PID" || fail "worker A exited before it held a lease"
  local leased
  leased="$(queue_company 2> /dev/null | jq -r '.depth.leased // 0')" || return 1
  [ "$leased" -ge 1 ] 2> /dev/null
}
require_wait "the queue to show a lease held by $WORKER_A" 30 a_holds_a_lease

HELD_RUN=""
for id in $COMPANY_RUNS; do
  owner="$(queue_company --run "$id" 2> /dev/null | jq -r '.lease.owner // ""')" || owner=""
  if [ "$owner" = "$WORKER_A" ]; then HELD_RUN="$id"; break; fi
done
[ -n "$HELD_RUN" ] || fail "the queue shows a leased run but none is leased by $WORKER_A"
note "$WORKER_A holds the lease on $HELD_RUN and is blocked on its first write to the ledger"

# A is only worth killing once Prometheus has scraped it, or check 4 could not
# say it ever did. The scrape works while the events table is locked because it
# only reads. A scrape that never comes is recorded by check 4, not treated as
# fatal here, so the takeover can still be shown.
a_scraped() { [ "$(prom_value "up{job=\"$PROM_JOB\",instance=\"host.docker.internal:$A_PORT\"}")" = "1" ]; }
if wait_until 40 1 a_scraped; then
  note "Prometheus scraped worker A while it held the run"
else
  note "Prometheus did not scrape worker A within 40s; check 4 will report it"
fi

# --------------------------------------------------------------------------
step "kill worker A with SIGKILL while it holds the run, then release the ledger"
kill -9 "$A_PID" 2> /dev/null || note "worker A had already exited"
{ wait "$A_PID"; } 2> /dev/null || true
A_PID=""
end_lock_session
lock_session_gone() {
  [ "$(psql_admin -Atq -c "SELECT count(*) FROM pg_stat_activity WHERE application_name = '$LOCK_APP'" 2> /dev/null)" = "0" ]
}
require_wait "the lock session to end" 20 lock_session_gone
note "worker A is dead; its lease on $HELD_RUN lapses within the 5s TTL"

# --------------------------------------------------------------------------
step "start worker B, which takes the run over once the lease lapses"
"$BIN" worker --manifest "$COMPANY_MANIFEST" --db "$(dsn "$COMPANY_DB")" --id "$WORKER_B" \
  --lease-ttl 5s --poll 200ms --metrics-addr "0.0.0.0:$B_PORT" --otlp-endpoint "$OTLP_ENDPOINT" \
  > "$WORK/worker-b.json" 2> "$WORK/worker-b.log" &
B_PID=$!

company_finished() {
  alive "$B_PID" || fail "worker B exited before the company database was drained"
  local finished
  finished="$(queue_company 2> /dev/null | jq -r '.depth.finished // 0')" || return 1
  [ "$finished" -ge 6 ] 2> /dev/null
}
require_wait "worker B to finish all 6 company runs" 120 company_finished
note "worker B finished the company runs, including the one worker A held"

# --------------------------------------------------------------------------
step "collect the evidence"

# 1. Every run completed. inspect reads the run record, so this is the ledger's
#    view and not the queue's.
statuses="$WORK/run-statuses.txt"
: > "$statuses"
completed=0
total=0
while read -r id database manifest; do
  [ -n "$id" ] || continue
  total=$((total + 1))
  status="$("$BIN" inspect "$id" --manifest "$manifest" --db "$(dsn "$database")" 2> /dev/null < /dev/null | jq -r '.run.status // "unreadable"')" ||
    status="unreadable"
  printf '%s %s %s\n' "$id" "$database" "$status" >> "$statuses"
  if [ "$status" = "completed" ]; then completed=$((completed + 1)); fi
done <<< "$RUN_TABLE"
if [ "$completed" -eq 8 ] && [ "$total" -eq 8 ]; then
  evidence 0 "all 8 runs completed" "$completed of $total runs have status completed (6 on $COMPANY_DB, 2 on $BILLING_DB)"
else
  evidence 1 "all 8 runs completed" "$completed of $total runs have status completed; see $statuses"
fi

# 2. The takeover is in the ownership log: A acquired the run, and B acquired it
#    later with the takeover flag set. The log is separate from the ledger, which
#    never records who executed a run.
queue_company --run "$HELD_RUN" > "$WORK/held-run-queue.json" 2> /dev/null || true
if jq -e --arg a "$WORKER_A" --arg b "$WORKER_B" '
    [.ownership[]? | select(.kind == "lease.acquired")] as $acquired
    | ($acquired | map(.owner) | index($a)) != null
      and ($acquired | map(select(.owner == $b and .detail.takeover == true)) | length) >= 1' \
  "$WORK/held-run-queue.json" > /dev/null 2>&1; then
  evidence 0 "the ownership log shows a takeover" "$HELD_RUN was acquired by $WORKER_A, then taken over by $WORKER_B (details in $WORK/held-run-queue.json)"
else
  evidence 1 "the ownership log shows a takeover" "no takeover of $HELD_RUN by $WORKER_B in $WORK/held-run-queue.json"
fi

# 3. Prometheus counted the takeover. The counter is B's own, so it is read per
#    worker label and never summed with the database-derived gauges that every
#    worker serves for the same database.
takeover_query="twinwright_worker_takeovers_total{worker=\"$WORKER_B\"}"
if wait_until 60 2 prom_at_least "$takeover_query" 1; then
  evidence 0 "Prometheus counted the takeover" "$takeover_query = $(prom_value "$takeover_query")"
else
  evidence 1 "Prometheus counted the takeover" "$takeover_query was not at least 1 within 60s (last value: $(prom_value "$takeover_query"))"
fi

# 4. Prometheus scraped both workers at some point. The window starts a few
#    seconds before worker A, so samples left by an earlier run of this demo
#    cannot satisfy it. A's target is down after its kill, which is expected; the
#    current value is reported for that reason and does not decide the check.
window=$(($(date +%s) - T_WORKERS + 5))
up_a="$(prom_value "max_over_time(up{job=\"$PROM_JOB\",instance=\"host.docker.internal:$A_PORT\"}[${window}s])")"
up_b="$(prom_value "max_over_time(up{job=\"$PROM_JOB\",instance=\"host.docker.internal:$B_PORT\"}[${window}s])")"
now_a="$(prom_value "up{job=\"$PROM_JOB\",instance=\"host.docker.internal:$A_PORT\"}")"
now_b="$(prom_value "up{job=\"$PROM_JOB\",instance=\"host.docker.internal:$B_PORT\"}")"
if [ "$up_a" = "1" ] && [ "$up_b" = "1" ]; then
  evidence 0 "Prometheus scraped both workers" \
    "up was 1 for worker A (port $A_PORT) and worker B (port $B_PORT); now A=${now_a:-none}, B=${now_b:-none}, and A's target is down after its kill, which is expected"
else
  evidence 1 "Prometheus scraped both workers" \
    "highest up in the last ${window}s: worker A=${up_a:-none}, worker B=${up_b:-none} (both must be 1)"
fi

# 5. Every run's trace is queryable in Jaeger by the trace ID the CLI computes
#    from the run ID. The workers export on completion, best effort, so each is
#    polled.
trace_id_of() {
  "$BIN" trace "$1" --db "$(dsn "$2")" --format json 2> /dev/null | jq -r '.trace_id // empty'
}
jaeger_has_trace() {
  curl -fsS --max-time 5 "$JAEGER_URL/api/traces/$1" 2> /dev/null |
    jq -e '((.data // []) | map(.spans | length) | add // 0) > 0' > /dev/null 2>&1
}
found=0
missing=""
while read -r id database _; do
  [ -n "$id" ] || continue
  trace_id="$(trace_id_of "$id" "$database" < /dev/null)" || trace_id=""
  if [ -n "$trace_id" ] && wait_until 60 2 jaeger_has_trace "$trace_id"; then
    found=$((found + 1))
  else
    missing="$missing $id"
  fi
done <<< "$RUN_TABLE"
if [ "$found" -eq 8 ]; then
  evidence 0 "every run's trace is queryable in Jaeger" "8 of 8 traces returned spans from $JAEGER_URL/api/traces/<trace ID>"
else
  evidence 1 "every run's trace is queryable in Jaeger" "$found of 8 traces found; missing:$missing"
fi

# 6. The benchmark gate, on SQLite in the work directory. It shares nothing with
#    the PostgreSQL databases above: it checks that the scripted suite still
#    scores what the approved baseline says.
if "$BIN" bench --suite standard --agent scripted --examples examples --out "$WORK/bench-candidate.json" \
  > "$WORK/bench.out" 2> "$WORK/bench.err" &&
  "$BIN" gate --baseline examples/bench/baselines/standard-scripted.json --candidate "$WORK/bench-candidate.json" \
    > "$WORK/gate.out" 2>&1; then
  evidence 0 "the benchmark gate passes" "the scripted standard suite matches examples/bench/baselines/standard-scripted.json"
else
  evidence 1 "the benchmark gate passes" "bench or gate failed; see $WORK/bench.err and $WORK/gate.out"
fi

# --------------------------------------------------------------------------
step "stop worker B"
# SIGINT cancels the worker's context; it releases any lease it holds and exits.
kill -INT "$B_PID" 2> /dev/null || true
if wait_until 30 1 exited "$B_PID"; then
  B_RC=0
  { wait "$B_PID"; } 2> /dev/null || B_RC=$?
  B_PID=""
  note "worker B stopped (exit status $B_RC)"
else
  note "worker B did not exit within 30s; the exit trap will stop it"
fi

step "summary"
printf '\nEvidence (%d checks, %d missing):\n%s\n' "$EVIDENCE_COUNT" "$EVIDENCE_FAILED" "$EVIDENCE"
printf 'Jaeger UI:  %s\n' "$JAEGER_URL"
printf 'Prometheus: %s\n' "$PROM_URL"
printf 'Work directory (worker logs, outcomes, manifests): %s\n' "$WORK"
printf 'The stack is still running. Stop it with:  make down   (this also removes the Compose services'"'"' data)\n'

if [ "$EVIDENCE_FAILED" -ne 0 ]; then
  printf '\ndemo-stack: %d of %d checks are missing\n' "$EVIDENCE_FAILED" "$EVIDENCE_COUNT" >&2
  exit 1
fi
printf '\ndemo-stack: all %d checks passed\n' "$EVIDENCE_COUNT"
