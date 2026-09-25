# ADR 0006: Versioned multi-service world definitions

Status: Accepted, 2026-09-25

Twinwright accepts a version 1 YAML world definition that names several OpenAPI service files and separate behavior binding files. Each service produces a normalized manifest; the world manifest combines them with entity and relationship metadata and a deterministic digest. Operation IDs remain unique across the world. The compiler rejects unsupported callable shapes and unregistered bindings instead of inferring business behavior from OpenAPI.

Entity fields and cross-service relationships are validated metadata. They do not create SQL foreign keys or seed new data automatically. The initial seed profile is `company-v1`, which runs the three company support scenarios against the existing deterministic fixture. New service behavior requires a Go handler registered under its explicit binding key. The dispatcher invokes that handler inside its existing transaction, retaining one commit for state, ledger, result, and transcript.

The original `build` path and its billing and company manifest digests remain compatible. `build-world` is the versioned path. It confines referenced files to the definition directory, including symlink resolution. Versioned manifests can be used by `run`, `resume`, `inspect`, and read-only `replay` when the seed profile supports the scenario.

This design does not treat interface schemas as executable business rules. Runtime seeding, behavior implementations, authorization, and distributed execution remain explicit code and later milestones.
