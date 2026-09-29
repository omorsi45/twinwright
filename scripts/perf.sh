#!/usr/bin/env bash
# Reproducible performance measurement for Twinwright's runtime primitives.
#
# This records the environment alongside the numbers, because a benchmark figure
# without the machine it ran on is not a measurement. Output is plain `go test
# -bench` format so `benchstat` can compare two files directly.
#
# Iteration counts are fixed rather than time-based (-benchtime=NNx), so two runs
# do the same amount of work and are comparable even when one machine is slower.
#
# Usage:
#   scripts/perf.sh                       # SQLite only
#   scripts/perf.sh "postgres://…"        # also measure PostgreSQL
#   ITERATIONS=200 COUNT=5 scripts/perf.sh
set -euo pipefail

DSN="${1:-}"
ITERATIONS="${ITERATIONS:-100}"
COUNT="${COUNT:-3}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Packages carrying benchmarks. Listed explicitly so a new package with slow
# tests cannot silently join a performance run.
PACKAGES=(./internal/store ./internal/worker ./internal/replay)

printf '# Twinwright performance run\n'
printf '# date: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf '# commit: %s\n' "$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
printf '# go: %s\n' "$(go version)"
printf '# os: %s\n' "$(uname -srm 2>/dev/null || echo unknown)"
printf '# iterations: %sx, count: %s\n' "$ITERATIONS" "$COUNT"
if [ -n "$DSN" ]; then
  printf '# postgres: enabled\n'
else
  printf '# postgres: skipped (no DSN argument)\n'
fi
printf '#\n'
printf '# Numbers below are this machine only. Do not quote them as project\n'
printf '# characteristics without the header above.\n\n'

# -run '^$' keeps the ordinary tests out of a timing run: a benchmark package
# that also runs its test suite reports the suite's wall clock as setup noise.
TWINWRIGHT_TEST_POSTGRES_DSN="$DSN" go test \
  -run '^$' \
  -bench . \
  -benchtime="${ITERATIONS}x" \
  -count="$COUNT" \
  -timeout 3600s \
  "${PACKAGES[@]}"
