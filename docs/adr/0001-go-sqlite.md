# ADR 0001: Go runtime with SQLite state

Status: Accepted, 2026-09-24

Use one Go process and SQLite for Milestone 1. Go provides a small deployable CLI and clear package boundaries. SQLite gives atomic local mutations and a persistent ordered ledger without operating a database server. A pure-Go driver keeps the build portable. Python integrations, PostgreSQL, and distributed execution can be added after the single-process contract is proven. SQLite write concurrency is an accepted limit.
