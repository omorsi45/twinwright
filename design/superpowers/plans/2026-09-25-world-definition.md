# Versioned World Definition Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Compile a versioned, multi-service world definition into a runnable manifest without adding each new operation route to the compiler's hardcoded map.

**Architecture:** Keep the legacy compiler path unchanged. Add a generic world compiler that parses several constrained OpenAPI services and explicit bindings, normalizes service manifests and relationship metadata, and computes a versioned digest. Route supported behavior keys through a Go registry while retaining the dispatcher's transaction boundary.

**Tech Stack:** Go 1.27.1, SQLite through modernc.org/sqlite, gopkg.in/yaml.v3, existing CLI and runner.

**Spec:** `C:\Users\Omar Morsi\Desktop\Projects\twinwright\design\superpowers\specs\2026-09-25-world-definition-design.md`

## Global constraints

- Start from commit `1093429` on `milestone-2-company-world` while PR 1 awaits merge. Do not alter the private `design/` Git ignore rule.
- Existing billing and company manifest digests, scenarios, replay, and `build` behavior must stay byte compatible.
- OpenAPI only declares interface shape; every callable operation has an explicit registered behavior binding.
- Every service mutation stays in the caller's SQL transaction and remains world-scoped and idempotent by call ID.
- World definition paths must be relative and confined to the definition directory.
- Tests use scripted/local providers only. No paid model call is required.

## File map

- `internal/behavior/registry.go`: handler signature, registration, built-in keys and adapters.
- `internal/dispatch/dispatch.go`: replace the prefix switch with registry lookup.
- `internal/compiler/world.go`: versioned YAML parsing, validation, service compilation, canonical digest.
- `internal/compiler/compiler.go`: optional service namespace and world metadata in a manifest; retain legacy compile path.
- `cmd/twinwright/main.go`: `build-world` command and seed-profile compatibility checks.
- `examples/company/world.yaml` and `examples/company/services/*`: first four-service definition.
- `docs/adr/0006-world-definition.md` and `README.md`: public contract and CLI usage.

## Review focus

- A renamed service operation with a duplicate global ID must fail before writing a manifest.
- A path escaping the world file directory must fail even when the file exists.
- Reordering YAML maps/lists that are semantically sets must not change the digest.
- A changed behavior binding or relationship must change the digest and fail resume/replay with the old manifest.
- A behavior registered under a different service module must fail compilation, not route to the wrong handler.

## Task 1: Handler registry without behavior switch

**Files:** Create `internal/behavior/registry.go` and `registry_test.go`; modify `internal/dispatch/dispatch.go`, `dispatch_test.go`.

**Interfaces:** `type Handler func(context.Context,*sql.Tx,string,string,string,string,map[string]any)(int,any,any,error)`; `Registry.Register(key string, handler Handler) error`; `Registry.Lookup(key string) (Handler,bool)`; `Builtin() *Registry`. `Dispatcher.Registry` is optional and defaults to built-ins.

- [ ] Add tests that register a new key, reject duplicates and blank keys, dispatch all four built-in modules, reject an unregistered behavior, and preserve read operations without `state.mutation`. The registry test should begin with `r := NewRegistry(); err := r.Register("example.read", handler)` and assert a second registration errors. Run `go test ./internal/behavior ./internal/dispatch -count=1`; observe the missing API failure.
- [ ] Implement a map lookup and explicit registrations for the current billing, CRM, ticketing, and messaging behavior keys. Adapt billing's typed mutation pointer to `any` only when non-nil:

```go
if mutation == nil { return status, body, nil, err }
return status, body, mutation, err
```

- [ ] Replace the dispatcher prefix switch with `handler, ok := registry.Lookup(op.Behavior)` and invoke it inside the existing transaction. Run focused tests and `go test ./internal/agent ./internal/replay -count=1`. Commit as `Register transactional service behaviors`.

## Task 2: Versioned world definition parser and metadata

**Files:** Create `internal/compiler/world.go`, `world_test.go`; modify `internal/compiler/compiler.go`.

**Interfaces:** `type WorldDefinition struct { Version int; Name, SeedProfile string; Services []ServiceSpec; Entities []EntitySpec; Relationships []RelationshipSpec }`; `type ServiceManifest struct { Name, Module, Digest string; Operations []Operation }`; `Manifest.World *WorldMetadata `json:"world,omitempty"``; `Operation.Service string `json:"service,omitempty"``.

- [ ] Write failing tests for empty/duplicate service names, version other than 1, unknown seed profile, unknown entity service/field, duplicate entity, and malformed `service.entity.field` relationship endpoints. Example valid relationship:

```yaml
relationships:
  - from: crm.accounts.customer_id
    to: billing.customers.id
```

- [ ] Parse YAML into typed structs, reject unknown fields, normalize sorted service/entity/relationship sets, and validate endpoint references. Export `ParseWorldDefinition(data []byte) (WorldDefinition,error)`. Run `go test ./internal/compiler -run TestWorldDefinition -count=1`. Commit as `Validate versioned world definitions`.

## Task 3: Generic multi-service compilation and digest

**Files:** Modify `internal/compiler/world.go`, `compiler.go`, tests in `world_test.go`.

**Interfaces:** `CompileWorld(definition []byte, load func(string)([]byte,error), registry *behavior.Registry) (Manifest,error)`; `ValidateManifestWithRegistry(Manifest,*behavior.Registry) error`. The existing `Compile` and `ValidateManifest` remain available.

- [ ] Add failing tests for two or more valid service specs; duplicate IDs across services; unknown binding; binding prefix different from service module; unsupported method/body/schema/`$ref`; changed bindings and relationships changing digest; reordered YAML producing the same digest. Assert the old billing digest remains `1e90724d491f5de49a1271faf2136ddaf214773cfc8f0e5bd82f41b430c6e340`.
- [ ] Reuse the existing constrained OpenAPI shape parser but factor route checking: legacy `Compile` keeps exact `routes` validation, world compilation validates generic primitive shapes and registered behavior keys. A normalized operation retains `ID`, `Service`, `Method`, `Path`, `Behavior`, `Required`, and `Properties`. Reject ambiguous paths and all unsupported shapes.
- [ ] Hash each canonical service manifest, then hash canonical world metadata plus sorted service manifests and flattened operations. `ValidateManifestWithRegistry` recomputes all hashes; default `ValidateManifest` uses built-ins for world manifests and its prior logic for legacy manifests. Run focused/full compiler tests and commit as `Compile multiple explicit service manifests`.

## Task 4: World example and CLI build

**Files:** Create `examples/company/world.yaml` and four service specs/bindings under `examples/company/services/`; modify `cmd/twinwright/main.go` and CLI tests.

**Interfaces:** `twinwright build-world examples/company/world.yaml --out company.world.manifest.json`; `run`, `resume`, and `replay` accept that manifest. `company-v1` allows the three company scenarios and rejects `duplicate-charge` when paired with the wrong profile.

- [ ] Add failing CLI tests that build the world manifest, run `company-incident` with `--agent scripted`, pause/resume, inspect, and replay; build rejects a file path containing `..` or an absolute path. Check a legacy manifest still runs and replays.
- [ ] Implement a CLI loader that rejects `filepath.IsAbs(path)`, `..` path segments, and resolved targets outside the world definition directory, then reads service files. Keep compiler I/O behind the injected loader. Validate seed profile before seeding. Run `go test ./cmd/twinwright -count=1` and full `go test -count=1 ./...`. Commit as `Build and run versioned company worlds`.

## Task 5: Documentation and end-to-end verification

**Files:** Add `docs/adr/0006-world-definition.md`; update `README.md` and `design/progress.md` in the ignored main checkout.

- [ ] Document world syntax, exact supported OpenAPI subset, explicit behavior registry, relationship metadata limits, CLI commands, and old-manifest compatibility. Include an example relationship and a note that new business behavior still requires Go code.
- [ ] Run `gofmt`, `go vet ./...`, `go test -count=1 ./...`, build the CLI, and smoke `build-world` plus all company scenarios and replay. Run `go test -race ./...` only if CGO and a C compiler are available; otherwise record the host limitation. Inspect `git diff --check` and the secret scan hook.
- [ ] Request independent read-only code review against this spec, fix Critical/Important findings, rerun affected checks, then commit docs. Keep personal working designs in ignored `design/`; publish only ADR and README.
