# Twinwright versioned world definition design

Date: 2026-09-25
Status: implementation design
Baseline: Milestone 2 branch `milestone-2-company-world`, commit `1093429`, PR 1

## Intent and boundary

Milestone 3 lets a developer assemble several OpenAPI service descriptions into one executable world without adding operation IDs and routes to the compiler's hardcoded map. OpenAPI supplies callable shapes. An explicit binding selects each implemented behavior. The compiler never invents business semantics. Existing billing and company manifests retain their byte-level digest contract and remain runnable and replayable.

## Format

A version 1 YAML world definition has a name, a `seed_profile`, a nonempty list of services, entity metadata, and relationship metadata. Each service names an OpenAPI file and a bindings file relative to the world definition. A service is a namespace and names its behavior module. Operation IDs must be unique across all services. The first example separates the current company API into billing, CRM, ticketing, and messaging specs while retaining the existing operation IDs. A behavior binding is a fully qualified key such as `crm.addAccountNote`. The service module must match the key prefix.

Entities identify a service, entity name, primary key field, and declared fields. Relationships use `service.entity.field` endpoints. Validation requires both endpoints to exist and reference declared fields; metadata does not claim to enforce SQL foreign keys. The initial supported seed profile is `company-v1`, mapped to the existing deterministic company scenario seeder. Unknown profiles fail explicitly.

## Compilation

`CompileWorld(definitionBytes, load)` parses the world definition and loads each referenced OpenAPI and bindings file through an injected loader. The CLI loader resolves paths under the definition's directory and rejects absolute or parent-traversal paths. The generic OpenAPI parser supports only the current primitive GET path parameters and POST JSON object bodies; `$ref`, unsupported schema keywords, other methods, ambiguous paths, duplicate operation IDs, unbound operations, and unknown behaviors fail. The legacy `Compile` path keeps its exact-route contract.

The executable manifest gains optional world metadata. Legacy manifests omit it and preserve their original digest calculation. A versioned world manifest contains one normalized service manifest and digest per service, plus a flattened operation list for the current runner. The world digest covers the canonical world metadata and sorted service manifests. Service and entity lists sort by name, fields and relationships sort, and each operation retains its path, method, arguments, behavior, and service namespace. Changing a binding, operation shape, entity relationship, world version, or seed profile changes the digest. `ValidateManifest` recalculates the appropriate digest and validates registered behavior keys.

## Runtime registry

The dispatcher uses a Go registry keyed by fully qualified behavior names. Built-in billing, CRM, ticketing, and messaging handlers register once in code. A handler receives the existing transaction, world ID, run ID, call ID, and arguments, then returns status, body, and optional mutation. The dispatcher still owns argument validation, fault injection, idempotency, event recording, saved result, transcript advance, and commit. A new service implementation adds one handler registration and explicit bindings; no central behavior switch grows with operation IDs. Unknown or duplicate registrations fail during setup.

The same runner, evaluator, and replay flow accept a versioned manifest when the seed profile supports the requested scenario. The first world example must run and replay all three company variants. Future seed profiles and handlers are extensions, not inferred from OpenAPI.

## Compatibility and failure behavior

Existing `build`, `run`, `resume`, and `replay` commands accept legacy manifests unchanged. A new `build-world` command compiles versioned definitions; `run` rejects a scenario incompatible with the manifest's seed profile. Resume and replay require the saved digest. Malformed world definitions and unsupported features fail before a run or world is created. Every handler remains world-scoped and transaction-local. The source of replay remains read-only.

## Verification

Tests cover canonical digest across reordered YAML, digest changes for meaningful edits, duplicate IDs across services, bad relationships and paths, unknown behaviors and modules, unsupported OpenAPI shapes, old digest compatibility, a world-defined company CLI run, fault and pause/resume, read-after-write state, and replay tamper detection. No paid model call is required.
