# ADR 0014: Twinwright Bench

Status: accepted, 2026-09-25

The roadmap asks for a serious public benchmark with machine-readable output and comparison, not a claim of thousands of trivial templates. Hard-coding cases in Go was rejected so contributors can add cases without touching the runner. Replaying recorded ledgers alone was rejected because the suite must exercise agents under controlled chaos, auth, and resume conditions.

A version 1 suite YAML lists cases with a category, world, scenario, optional chaos/auth/assertion paths, and dimension tags used for aggregation. Paths stay under `examples/`. The runner compiles the billing and company manifests once, executes each case in its own SQLite database, judges with assertion files when present else scenario evaluation, and writes one JSON report. Aggregate rates are measurements only. `compare` accepts two report files and emits deltas without declaring a winner. Run-id fork comparison is unchanged.

The first `standard` suite has 18 curated cases. Scripted fixtures are the verified CI path. Live providers may be selected with the same flags as `run`, and no live bench has been verified in this repository.
