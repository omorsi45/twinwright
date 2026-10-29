# Final Milestone: Twinwright Cloud Platform Implementation Plan

**Goal:** Deliver milestones 2 to 7 of the design on one branch: a regression gate with reproducible reports, real-provider recording and offline reconstruction, observability proven against a real collector and Prometheus, a bring-your-own-agent protocol with an isolation boundary, reproducible benchmarks, and Twinwright Cloud packaging.

**Architecture:** Extend existing packages (`bench`, `agent`, `replay`, `trace`, `metrics`, `worker`) and add focused new ones (`buildinfo`, `gate`, `perf`); add only the infrastructure a test needs (collector, Jaeger, Prometheus, Ollama) in Compose and GitHub Actions. Each part lands with its ADR.

**Tech Stack:** Go 1.27.3, SQLite, PostgreSQL, OpenTelemetry Collector, Jaeger, Prometheus, Ollama, Docker, Python 3 (reference agent, standard library only).

**Spec:** `docs/superpowers/specs/2026-10-26-twinwright-cloud-design.md` (Milestones 2 to 7 and "Final milestone: scope decisions" are binding).

## Global Constraints

- Work on branch `cloud/m2-platform` (stacked on `cloud/m1-reliability-core`) from the repository root. Use Go 1.27.3 and a working git on PATH.
- No new Go module dependencies (`require` lines unchanged); `go mod tidy` produces no diff.
- `go test ./...` stays hermetic: no network, API key, Docker, PostgreSQL, Ollama or Python requirement. External dependencies are opt-in by environment variable: `TWINWRIGHT_TEST_POSTGRES_DSN`, `TWINWRIGHT_TEST_DOCKER=1`, `TWINWRIGHT_TEST_OTEL=1`, `TWINWRIGHT_TEST_LIVE_OPENAI=1`, `TWINWRIGHT_TEST_LIVE_ANTHROPIC=1`; a test needing `python3` skips when `exec.LookPath("python3")` fails.
- Each commit passes `make check` and `go test -count=1 ./...`; the last task also passes `go test -race -count=1 ./...`, `./scripts/e2e.sh`, `make bench-smoke` and `twinwright demo`.
- Preserve the `AGENTS.md` invariants and milestone 1's published contract (ADR 0029). The ledger never records worker identity. Delivery is at-least-once, never described as exactly-once.
- Existing CLI behaviour, flags, JSON fields and exit codes are unchanged unless a task says otherwise; new behaviour is opt-in.
- Secrets (API keys, configured secret values, `Authorization`, `x-api-key`) never reach the ledger, cassettes, traces, logs or reports.
- Every guarantee change gets an ADR (next numbers: 0030 gate, 0031 provider recording, 0032 observability, 0033 agent protocol, 0034 benchmarks).
- Written text: no em dashes, claims only what a test or CI job proves (say "not measured" or "not verified" otherwise), no assistant attribution or `Co-authored-by` trailer. Commit messages explain why.
- Never weaken an invariant or a test to get green; report BLOCKED with evidence instead.

## Review Focus

- A baseline report written by v0.1.0 or milestone 1 (no `schema_version`): `gate` must load it as version 1 and compare what both versions carry, not crash or pass vacuously. Pinned by `TestGateComparesAVersionOneBaseline` (Task 3).
- A candidate suite with a case the baseline lacks, or missing one it has: a missing case is a regression, a new case is reported, never silently ignored. Pinned by `TestGateReportsMissingAndNewCases` (Task 3).
- A provider that answers 429 with `Retry-After: 120`: the wait is capped at 30 seconds and recorded, and a cancelled context ends the wait at once. Pinned by `TestRetryAfterIsCappedAndCancellable` (Task 6).
- A cassette replayed against a run whose request differs (model or history changed): replay must fail naming the exchange, never fall through to the network. Pinned by `TestCassetteDivergenceNamesTheExchange` (Task 7).
- An agent process that writes non-JSON, exits non-zero, hangs, or floods stdout: the turn fails with a bounded, redacted error and the process is killed. Pinned by `TestExecProviderRejectsMisbehavingAgents` (Task 12).

## File Structure

| Area | Files |
|---|---|
| Build metadata | `internal/buildinfo/buildinfo.go` (new); `cmd/twinwright/version.go` |
| Reports and gate | `internal/bench/report.go`, `run.go`, `suite.go`, `fingerprint.go` (new); `internal/gate/` (new); `cmd/twinwright/gate.go` (new); `examples/bench/baselines/standard-scripted.json` (new) |
| Providers | `cmd/twinwright/providers.go` (new, `providerConfig`, `selectProvider`); `internal/agent/{openai,chat,anthropic,interrupt,runner}.go`; `internal/agent/cassette.go` (new); `internal/redact/redact.go`; `internal/replay/replay.go` |
| Agent protocol | `internal/agent/exec.go` (new); `docs/protocol/` (new); `examples/agents/python/` (new) |
| Observability | `internal/trace/{format,export}.go`; `internal/metrics/metrics.go`; `internal/worker/worker.go`; `deploy/`; `docker-compose.yml` |
| Benchmarks | `internal/perf/` (new); `cmd/twinwright/perf.go` (new); `docs/performance.md`; `docs/benchmarks/` (new) |
| CI and demo | `.github/workflows/{ci,integration}.yml`; `scripts/{live-model,demo-stack}.sh` (new); `Makefile`; `cmd/twinwright/demo.go` |
| Docs | `docs/adr/0030-0034`; `docs/architecture.md` (new); `README.md`; `CHANGELOG.md`; `docs/security-review.md` |

---

## Part A: Regression gate and reproducible reports

### Task 1: Build metadata and bench report schema version 2

**Files:**
- Create: `internal/buildinfo/buildinfo.go`, `internal/buildinfo/buildinfo_test.go`
- Modify: `cmd/twinwright/version.go` (use buildinfo; add `revision` and `modified` to `version` output), `internal/bench/report.go`, `internal/bench/run.go`, `internal/bench/suite.go`
- Test: `internal/bench/report_test.go`, `internal/bench/suite_test.go`, `cmd/twinwright/main_test.go`

**Interfaces:**
- Produces:

```go
package buildinfo
type Info struct {
    Version   string `json:"twinwright_version"`
    Revision  string `json:"git_revision,omitempty"` // vcs.revision from debug.ReadBuildInfo
    Modified  bool   `json:"git_modified"`            // vcs.modified
    GoVersion string `json:"go_version"`
}
func Read(version string) Info // version is cmd's main.Version (ldflags); falls back as version() does today

// internal/bench
const ReportSchemaVersion = 2
type Environment struct {
    buildinfo.Info
    GOOS      string `json:"goos"`
    GOARCH    string `json:"goarch"`
    NumCPU    int    `json:"num_cpu"`
    Seed      int64  `json:"seed"`
    StartedAt string `json:"started_at"` // RFC 3339 UTC
}
// Report gains: SchemaVersion int `json:"schema_version"`; ContentDigest string `json:"content_digest"`; Environment *Environment `json:"environment,omitempty"`
// Options gains: Build buildinfo.Info
func (s Suite) ContentDigest(examplesRoot string) (string, error) // sha256 over Digest() plus, in sorted path order, each referenced file's path and bytes (chaos, auth, assertions, interventions)
```

- `DecodeReport` sets `SchemaVersion = 1` when the field is absent.

- [ ] **Step 1: Write failing tests:**
  - `TestReportCarriesSchemaVersionAndEnvironment`: after `bench.Run` on the standard suite with `Seed: 7`, the report has `SchemaVersion == 2`, `Environment.Seed == 7`, `Environment.GOOS == runtime.GOOS`, `NumCPU > 0`, `StartedAt` parses as RFC 3339, and `ContentDigest != ""`.
  - `TestContentDigestCoversFixtureBytes`: copy `examples` to `t.TempDir()`, change one byte of an assertion file a case references, and want the same `Digest()` but a different `ContentDigest`.
  - `TestDecodeReportTreatsMissingSchemaVersionAsOne`: decoding `{"suite":"s","cases":[]}` gives `SchemaVersion == 1`.
  - `TestBuildinfoReadsVersionFallback`: `Read("")` has a non-empty `GoVersion` and `Version` non-empty ("dev" or module version).
- [ ] **Step 2:** `go test -count=1 ./internal/bench/ ./internal/buildinfo/` gives FAIL (undefined).
- [ ] **Step 3:** Implement. `cmd/twinwright` passes `buildinfo.Read(Version)` into `bench.Options.Build`. The `version` command adds `git_revision` and `git_modified` and keeps its existing fields.
- [ ] **Step 4:** `go test -count=1 ./internal/bench/ ./internal/buildinfo/ ./cmd/twinwright/` gives PASS.
- [ ] **Step 5:** Commit. The why: two reports could not be compared without knowing what produced them.

### Task 2: Per-case fingerprint and trials

**Files:**
- Create: `internal/bench/fingerprint.go`, `internal/bench/fingerprint_test.go`
- Modify: `internal/bench/report.go` (CaseResult fields), `internal/bench/run.go` (fill them; trials loop for local mode), `internal/bench/suite.go` (`trials` case field), `cmd/twinwright/main.go` (bench `--trials` flag, default 1)

**Interfaces:**
- Produces:

```go
type Step struct { Operation string `json:"operation"`; Status int `json:"status"` }
// CaseResult gains:
//   Trajectory []Step `json:"trajectory,omitempty"` // from tool.response events of the case's run, in seq order
//   Mutations int `json:"mutations"`                // state.mutation events (chaos.actor_mutation excluded)
//   SuccessfulViolations int `json:"successful_violations,omitempty"` // from eval.AnalyzeRun security analysis when present
//   Trials int `json:"trials"`; Passes int `json:"passes"`
func Fingerprint(ctx context.Context, s *store.Store, runID string) (trajectory []Step, mutations int, err error)
```

- Trials: Options gains `Trials int` (0 means 1), and a suite case may set `trials: N`; the case value wins. `trials > 1` on a `distributed` or `counterfactual` case is a parse error naming the field. With trials N, a local case runs N times, each in a fresh DB named `<id>-<trial>.db`. `Passes` counts passing trials, and `Status` is "passed" only when every trial passed. Trajectory, mutations and run ID come from the first trial.

- [ ] **Step 1: Write failing tests:**
  - `TestFingerprintOfTheBillingRun`: a scripted duplicate-charge run gives a trajectory with operations `getCustomer, listInvoices, listCharges, createRefund`, last status 201 and `mutations == 1`.
  - `TestFingerprintExcludesActorMutations`: on the `reliability-concurrent-mutation` case of the standard suite, `Mutations` equals the count of `state.mutation` only.
  - `TestTrialsRunEachLocalCaseNTimes`: a one-case suite with `trials: 3` gives `Trials == 3` and `Passes == 3`.
  - `TestTrialsRefusedOutsideLocalMode`: `trials: 2` on a distributed case gives a parse error containing `trials`.
- [ ] **Step 2:** Run and confirm FAIL. **Step 3:** Implement. **Step 4:** `go test -count=1 ./internal/bench/ ./cmd/twinwright/` gives PASS. **Step 5:** Commit.

### Task 3: The gate

**Files:**
- Create: `internal/gate/gate.go`, `internal/gate/junit.go`, `internal/gate/markdown.go`, `internal/gate/gate_test.go`, `cmd/twinwright/gate.go`
- Modify: `cmd/twinwright/main.go` (dispatch `gate`; usage string), `cmd/twinwright/main_test.go`

**Interfaces:**
- Consumes: `bench.Report`, `bench.DecodeReport`, the CaseResult fields from Tasks 1 and 2.
- Produces:

```go
package gate
type Options struct {
    Tolerance  float64 // allowed drop in a case's pass rate, 0..1, default 0
    Trajectory string  // "fail" | "warn" (default) | "ignore"
}
type Finding struct { Case string `json:"case"`; Kind string `json:"kind"`; Detail string `json:"detail"` }
type Result struct {
    Passed       bool      `json:"passed"`
    Baseline     string    `json:"baseline_suite"`
    Candidate    string    `json:"candidate_suite"`
    Regressions  []Finding `json:"regressions"`
    Warnings     []Finding `json:"warnings"`
    Improvements []Finding `json:"improvements"`
    Compared     int       `json:"cases_compared"`
}
func Compare(baseline, candidate bench.Report, opts Options) (Result, error) // error only when suites differ by name
func (r Result) Markdown() string
func (r Result) JUnit() ([]byte, error)
```

Regression kinds, exactly:
- `case_missing`
- `newly_failing`: the baseline passed, and the candidate failed or errored.
- `pass_rate_drop`: the candidate's `Passes/Trials` is below the baseline's minus `Tolerance`.
- `control_undetected`: an `expect: fail` case that was as expected and no longer is.
- `extra_mutations`: candidate `Mutations` greater than the baseline's.
- `new_violation`: candidate `SuccessfulViolations` greater.
- `new_unsafe_retry`: newly true.
- `new_duplicate_refund`: `AgentRefunds` greater than the baseline's.

Trajectory differences have kind `trajectory_changed`. They are a warning by default, a regression when `Trajectory == "fail"`, and skipped when `"ignore"`. A case only in the candidate is a warning of kind `case_added`. A content digest difference is a warning of kind `suite_changed`. Fields absent from a version 1 report are not compared. `Passed` means there are no regressions.

CLI: `twinwright gate --baseline A.json --candidate B.json [--tolerance 0] [--trajectory warn] [--markdown PATH] [--junit PATH]`.
- It prints the Result JSON to stdout.
- It exits 1 when there are regressions, and 2 on unreadable input or a suite name mismatch.
- `PATH` may be `-` for stdout.

- [ ] **Step 1: Write failing tests** in `gate_test.go`, built on in-memory reports:
  - `TestGatePassesIdenticalReports`
  - `TestGateFlagsNewlyFailingCase`: Detail names the failed checks.
  - `TestGateFlagsUndetectedControl`
  - `TestGateFlagsExtraMutationsAndDuplicateRefunds`
  - `TestGateToleranceAllowsSmallPassRateDrop`: baseline 10/10, candidate 9/10; tolerance 0.1 passes and 0.05 fails.
  - `TestGateTrajectoryModes`
  - `TestGateReportsMissingAndNewCases`
  - `TestGateComparesAVersionOneBaseline`: a v1 report with no trials or mutations compares status and controls only.
  - `TestGateRefusesDifferentSuites`
  - `TestJUnitHasOneTestcasePerCaseAndFailuresForRegressions`: parse it with `encoding/xml`.
  
  In `main_test.go`: `TestGateExitCodes`, which wants 0 for identical reports, 1 for a regression and 2 for a missing file.
- [ ] **Step 2:** Run and confirm FAIL. **Step 3:** Implement. **Step 4:** `go test -count=1 ./internal/gate/ ./cmd/twinwright/` gives PASS. **Step 5:** Commit.

### Task 4: Deliberate regression, approved baseline, CI gate, demo step, ADR 0030

**Files:**
- Create: `internal/agent/regressed.go`, `internal/agent/regressed_test.go`, `examples/bench/baselines/standard-scripted.json`, `docs/adr/0030-regression-gate.md`
- Modify: `cmd/twinwright/main.go`. This covers three things:
  - `selectProvider`: when the scripted model is `fixture-regressed-v1`, wrap the scenario's fixture.
  - bench: when `--agent scripted --model fixture-regressed-v1`, use that model for every case.
  - `run --fail-on-eval`.
- Also modify: `internal/bench/run.go` (`scriptedModel` honours `Options.Model == "fixture-regressed-v1"`), `cmd/twinwright/demo.go` (step 12), `.github/workflows/ci.yml` (a `regression-gate` job), `Makefile` (`gate` target), `CHANGELOG.md`

**Interfaces:**
- Produces: `type RegressedProvider struct { Inner Provider; Manifest compiler.Manifest }`. Its `Next` returns the inner decision, except for the first turn whose tool call targets an operation whose `Method != "GET"`. For that turn it returns `Message{Role: "assistant", Content: "The issue is resolved."}` with no tool calls.

- [ ] **Step 1: Write failing tests:**
  - `TestRegressedProviderStopsBeforeTheFirstWrite`: a billing run completes without `createRefund`, and the duplicate-charge evaluation fails.
  - `TestRegressedBenchFailsTheGate` (cmd): bench standard with the scripted agent, then gate against `examples/bench/baselines/standard-scripted.json`. Want exit 0 for `fixture-v1` and exit 1 for `fixture-regressed-v1`, with a regression for case `billing-duplicate-charge`.
  - `TestRunFailOnEval`: an unsafe ambiguous-commit `run --fail-on-eval` returns an error, and without the flag it returns nil.
  - Extend `TestDemo` to expect 12 steps. Step 12 benches the regressed fixture, gates it, and its evidence names the regressed case count.
- [ ] **Step 2:** Run and confirm FAIL.
- [ ] **Step 3:** Implement, then generate the baseline with `go run ./cmd/twinwright bench --suite standard --agent scripted --examples examples --out examples/bench/baselines/standard-scripted.json`.
  - The CI job builds, benches into `candidate.json`, and runs `gate --baseline examples/bench/baselines/standard-scripted.json --candidate candidate.json --markdown "$GITHUB_STEP_SUMMARY"`.
  - It then benches with `fixture-regressed-v1` and requires `gate` to exit 1, so the gate itself is proven able to fail.
  - The ADR records the kinds, the tolerance semantics, and the rule that CI gates only deterministic and cassette reports.
- [ ] **Step 4:** `go test -count=1 ./... && ./scripts/e2e.sh && go run ./cmd/twinwright demo --dir "$(mktemp -d)/d"` gives PASS, with demo 12/12.
- [ ] **Step 5:** Commit.

## Part B: Real providers, recording and offline reconstruction

### Task 5: Provider configuration, response metadata, output limits and redaction

**Files:**
- Create: `cmd/twinwright/providers.go`. Move `selectProvider` and `providerTimeoutFlag` here, and add the flag helper.
- Modify: `cmd/twinwright/main.go`, `cmd/twinwright/distributed.go` (every `selectProvider` call site: run, resume, fork, counterfactual, bench, shadow, worker), `internal/agent/{openai,chat,anthropic,runner}.go`, `internal/redact/redact.go`
- Test: `internal/agent/metadata_test.go`, `internal/redact/redact_test.go`, `cmd/twinwright/main_test.go`

**Interfaces:**
- Produces:

```go
// cmd/twinwright
type providerConfig struct {
    Name, Model, Scenario, BaseURL string
    Timeout         time.Duration
    MaxOutputTokens int    // 0 means the adapter default
    Cassette        string // Task 7
    RecordCassette  string // Task 7
    AgentCommand    string // Task 12
    AgentEnv        []string
    AgentContainer  agentContainerFlags // Task 13
}
func selectProvider(cfg providerConfig) (agent.Provider, error)
func providerFlags(fs *flag.FlagSet) *providerConfig // registers --base-url, --provider-timeout, --max-output-tokens (and later flags), with today's names and defaults

// internal/agent: Message gains, all omitempty:
//   ResponseID string `json:"response_id,omitempty"`; ServerModel string `json:"server_model,omitempty"`
//   FinishReason string `json:"finish_reason,omitempty"`; HTTPStatus int `json:"http_status,omitempty"`
// OpenAIProvider and ChatCompletionsProvider gain MaxOutputTokens int (sent as max_output_tokens / max_tokens only when > 0).
const MaxResponseBytes = 4 << 20
var ErrResponseTooLarge = errors.New("provider response exceeds 4 MiB")

// internal/redact
func Secrets(s string, secrets ...string) string // masks only the given secret values (8+ chars); no patterns
func URL(raw string) string                      // strips userinfo and the whole query string
```

Behaviour:
- Each adapter decodes the response id, server model and finish/stop reason on success, and sets `HTTPStatus`.
- The body is read with a limit of `MaxResponseBytes+1`. Exceeding it returns `ErrResponseTooLarge`, which is not retried.
- On success, `RawOutput` and `Content` pass through `redact.Secrets(…, APIKey)`.
- Transport errors (a `*url.Error` from `Do`) are rewritten so their URL goes through `redact.URL` and their text through `redact.String(…, APIKey)`.

- [ ] **Step 1: Write failing tests:**
  - `TestAdaptersRecordResponseMetadata`: one subtest per adapter against `httptest`, wanting the id, model, finish reason and 200.
  - `TestOversizedResponseIsAnExplicitError`: a 5 MiB body gives `errors.Is(err, agent.ErrResponseTooLarge)`.
  - `TestSuccessfulOutputIsRedacted`: the model echoes the configured key, and `RawOutput` has no key.
  - `TestTransportErrorHidesURLQueryAndUserinfo`: the base URL is `http://user:pw@127.0.0.1:1/v1?key=abc`, and the error contains neither `pw` nor `abc`.
  - `TestMaxOutputTokensReachesTheRequest`: one subtest per adapter.
  - `TestProviderFlagsKeepTheirNamesAndDefaults` (cmd): `--provider-timeout` defaults to 90s and `--base-url` defaults to `$OPENAI_BASE_URL`.
- [ ] **Step 2:** Run and confirm FAIL. **Step 3:** Implement. **Step 4:** `go test -count=1 ./... && ./scripts/e2e.sh` gives PASS. **Step 5:** Commit.

### Task 6: Retry rate limits, server errors and transport failures

**Files:**
- Modify: `internal/agent/interrupt.go`, `internal/agent/{openai,chat,anthropic}.go`, `internal/agent/runner.go`
- Test: `internal/agent/retry_test.go`

**Interfaces:**
- Produces: new kinds `InterruptRateLimited = "rate_limited"`, `InterruptServerError = "server_error"` and `InterruptTransport = "transport"`.
  - `CallInterrupted` gains `Status int` and `RetryAfter time.Duration`.
  - `Interruption` gains `Status int json:"status,omitempty"` and `RetryAfterMS int json:"retry_after_ms,omitempty"`.
  - `Runner` gains `Sleep func(ctx context.Context, d time.Duration) error`. Nil means a context-aware timer.
  - `const MaxRetryAfter = 30 * time.Second`.
- Behaviour:
  - Adapters return `*CallInterrupted` as follows:
    - Rate limited: HTTP 429, with `Retry-After` parsed as seconds or as an HTTP date.
    - Server error: HTTP 500, 502, 503 or 504.
    - Transport: a `Do` error while the caller's context is live and the per-attempt deadline has not passed.
  - Every other non-2xx response is an ordinary error, as today.
  - `callProvider` keeps `providerAttempts = 2`. Before the second attempt it waits `min(RetryAfter, MaxRetryAfter)`, or 1s when no `Retry-After` was sent. Timeouts and truncations keep retrying immediately, as today.
  - The wait goes through `Runner.Sleep`. If the context ends during the wait, the call returns the context error.
  - Each interruption is recorded exactly like today's kinds, so replay reproduces it from the turn.

- [ ] **Step 1: Write failing tests:**
  - `TestRateLimitedAttemptIsRetriedAndRecorded`: the server answers 429 with `Retry-After: 2`, then 200. The run turn succeeds, and the ledger has `provider.interrupted` with `{"kind":"rate_limited","attempt":1,"status":429,"retry_after_ms":2000}`. The fake Sleep was called with 2s.
  - `TestServerErrorIsRetriedOnce`
  - `TestRetryAfterIsCappedAndCancellable`: `Retry-After: 120` makes the fake Sleep receive 30s. A real Sleep with a cancelled context returns `context.Canceled` immediately.
  - `TestClientErrorIsNotRetried`: a 400 gives one attempt.
  - `TestRetriedRunStillReplays`: `replay.Verify` passes on the 429-then-200 run.
- [ ] **Step 2:** Run and confirm FAIL. **Step 3:** Implement. **Step 4:** `go test -count=1 ./internal/agent/ ./internal/replay/ ./...` gives PASS. **Step 5:** Commit.

### Task 7: Cassettes: record wire exchanges, reconstruct offline through the adapter

**Files:**
- Create: `internal/agent/cassette.go`, `internal/agent/cassette_test.go`
- Modify: `internal/replay/replay.go` (`VerifyWith`), `cmd/twinwright/providers.go` (`--record-cassette`, `--cassette`), `cmd/twinwright/main.go` (`replay --cassette`)
- Test: `internal/replay/cassette_test.go`, `cmd/twinwright/main_test.go`

**Interfaces:**
- Produces:

```go
const CassetteVersion = 1
type Exchange struct {
    Seq           int             `json:"seq"`
    Method        string          `json:"method"`
    URL           string          `json:"url"`            // redact.URL applied
    RequestSHA256 string          `json:"request_sha256"` // of the request body bytes as sent
    Request       json.RawMessage `json:"request"`        // body, redact.Secrets applied
    Status        int             `json:"status"`
    RetryAfter    string          `json:"retry_after,omitempty"`
    Response      string          `json:"response"`       // body, redact.Secrets applied
}
// File format: JSON lines; first line {"twinwright_cassette":1,"provider":..., "model":...}, then one Exchange per line, flushed per exchange.
func NewRecordingTransport(base http.RoundTripper, path, provider, model string, secrets ...string) (*RecordingTransport, error) // RoundTrip records then returns the real response
func LoadCassette(path string) (*Cassette, error)
func (c *Cassette) Transport() http.RoundTripper // serves exchanges in order; never dials
var ErrCassetteDivergence = errors.New("cassette divergence")
// replay
func VerifyWith(ctx context.Context, source *store.Store, runID string, manifest compiler.Manifest, provider agent.Provider) (Report, error) // Verify == VerifyWith(..., recordedProvider)
```

- Divergence: when the next request's method, the URL path, or the sha256 of the request body differs from the next exchange, the transport returns an error wrapping `ErrCassetteDivergence`. The error names the seq and the differing field. An exhausted cassette also diverges.
- Only the response headers `Content-Type` and `Retry-After` are replayed. Request headers are never recorded.
- With `--cassette`, the CLI builds the run's own adapter (provider and model from the run record), uses `Client: &http.Client{Transport: cassette.Transport()}` and a placeholder key. `replay <run> --cassette F` calls `VerifyWith` with it.

- [ ] **Step 1: Write failing tests:**
  - `TestRecordingThenCassetteReplayReproducesTheLedger`: record a chat-completions run against an `httptest` scripted server (reuse the `TestChatCompletionsToolRoundTrip` server). Close the server, then `VerifyWith` using the cassette transport gives verified.
  - `TestCassetteNeverDials`: a transport base that fails the test if called.
  - `TestCassetteDivergenceNamesTheExchange`: an altered history gives an error containing `seq 2` and `request_sha256`.
  - `TestCassetteHoldsNoSecrets`: the key and the `Authorization` value are absent from the file bytes.
  - `TestReplayCassetteFromCLI`.
- [ ] **Step 2:** Run and confirm FAIL. **Step 3:** Implement. **Step 4:** `go test -count=1 ./internal/agent/ ./internal/replay/ ./cmd/twinwright/` gives PASS. **Step 5:** Commit.

### Task 8: A real model run, live provider tests, ADR 0031

**Files:**
- Create: `scripts/live-model.sh`, `internal/agent/live_test.go`, `docs/adr/0031-provider-recording-and-real-models.md`, `docs/live-model-run.md`
- Modify: `.github/workflows/integration.yml` (job `real-model`), `Makefile` (`live-model` target), `CHANGELOG.md`

**Behaviour:**
- `scripts/live-model.sh [BASE_URL] [MODEL]` defaults to `http://127.0.0.1:11434/v1` and `$TW_LIVE_MODEL`. It:
  - builds the CLI and the billing manifest;
  - runs `duplicate-charge` with `--agent openai-compatible --record-cassette` into a temp dir with `--steps 12`;
  - prints the evaluation verdict;
  - runs `replay <run>` (ledger, no provider) and `replay <run> --cassette`;
  - writes `live-model-report.json` with model, base URL host, evaluation passed, model turns, tool calls, replay verdicts, cassette exchange count and wall time.
- It exits non-zero only if recording, ledger replay or cassette replay fails. The model's evaluation verdict is reported, not gated.
- CI job `real-model`:
  - starts `ollama/ollama` (pinned tag) with `docker run -d`;
  - pulls the model chosen at implementation time (recording its digest from `/api/tags` in the report);
  - runs the script;
  - stops the container (`docker stop`) before a second cassette replay, proving the replay needs no server;
  - uploads the report and cassette as artifacts.
- `live_test.go`: `TestLiveOpenAIToolCall` and `TestLiveAnthropicToolCall`, gated on `TWINWRIGHT_TEST_LIVE_OPENAI=1`/`OPENAI_API_KEY` and `TWINWRIGHT_TEST_LIVE_ANTHROPIC=1`/`ANTHROPIC_API_KEY` plus a model env var. Each makes one real call offering `getCustomer` and asserts a well-formed message with a non-empty `ResponseID`.
- `docs/live-model-run.md` records the actual local run on the development Mac (model, digest, machine, outcome, timings), pass or fail, taken from the report the script wrote. The controller runs the script locally after the implementer finishes and dispatches the doc update with the report path.

- [ ] **Step 1:** Write `live_test.go`, and confirm both tests skip without the env vars (`go test -count=1 -run Live -v ./internal/agent/` prints SKIP).
- [ ] **Step 2:** Write the script and the job. Run the script locally against the development Mac's Ollama (`ollama pull <model>` first) and confirm it writes a report with both replay verdicts true.
- [ ] **Step 3:** Write the ADR: cassette format, redaction, retry kinds, metadata fields, what a live run does and does not prove, and the cut of error-run replay.
- [ ] **Step 4:** `go test -count=1 ./...` gives PASS. **Step 5:** Commit. Do not commit `live-model-report.json` or cassettes except the doc's quoted figures.

## Part C: Observability against a real collector and Prometheus

### Task 9: Trace correlation and worker export

**Files:**
- Modify: `internal/trace/format.go` (span attributes), `internal/trace/export.go` (`Delivery.TraceID`), `internal/trace/trace.go` (exported `TraceIDFor`), `internal/worker/worker.go` (`Outcome.TraceID` and the log attribute), `cmd/twinwright/distributed.go` (worker `--otlp-endpoint`, `--otlp-header`)
- Test: `internal/trace/format_test.go`, `internal/trace/export_test.go`, `cmd/twinwright/main_test.go`

**Interfaces:**
- `func TraceIDFor(runID string) string` is the same derivation `Build` uses today.
- Every OTLP span carries int attributes `twinwright.event.start_seq` and `twinwright.event.end_seq`, plus `twinwright.run_id`.
- `Delivery` gains `TraceID string json:"trace_id"`.
- The worker's "claim finished" log line gains `trace_id`.
- When a worker has `--otlp-endpoint` set and a claim ends `Finished` with run status `completed`, the worker builds and exports that run's trace.
  - The export is best effort: a failure is logged at warn level with the trace ID and never changes the disposition.
  - `Outcome` gains `TraceExported bool json:"trace_exported,omitempty"`.
- [ ] **Step 1: Write failing tests:**
  - `TestOTLPSpansCarryLedgerSequence`: every span in a scripted run's OTLP export has both seq attributes, and the root span's start is 1.
  - `TestDeliveryReportsTheTraceID`
  - `TestWorkerExportsACompletedRunsTrace`: a worker command with an `httptest` collector drains one run, the collector receives one POST whose traceId equals `TraceIDFor(runID)`, and the outcome has `trace_exported`.
  - `TestWorkerExportFailureDoesNotFailTheRun`
- [ ] **Step 2:** Run and confirm FAIL. **Step 3:** Implement. **Step 4:** `go test -count=1 ./internal/trace/ ./internal/worker/ ./cmd/twinwright/` gives PASS. **Step 5:** Commit.

### Task 10: Metrics that are complete and correctly labelled

**Files:**
- Modify: `internal/metrics/metrics.go`, `internal/worker/worker.go` (`Config.OnRenew func(store.RunLease)`, called after each successful renewal), `cmd/twinwright/distributed.go` (wire renewals and the histogram)
- Test: `internal/metrics/metrics_test.go`, `internal/worker/worker_test.go`, `cmd/twinwright/main_test.go`

**Interfaces:**
- Each counter has a declared label name. `WorkerDispositions` uses `disposition`; the others have none.
- Every counter renders a zero sample before its first `Add`. `WorkerDispositions` renders all four dispositions at 0.
- New metrics:
  - `twinwright_worker_run_duration_seconds`: a histogram with buckets 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120 and +Inf, labelled `worker`.
  - `twinwright_queue_oldest_runnable_age_seconds`: a gauge, 0 when nothing is runnable.
  - `twinwright_worker_last_seen_age_seconds{worker}`: a gauge per registered worker.
- `twinwright_worker_lease_renewals_total` is incremented through `OnRenew`.
- [ ] **Step 1: Write failing tests:**
  - `TestCountersRenderZeroBeforeFirstUse`
  - `TestCounterLabelNamesAreDeclaredPerMetric`: no `disposition` label on claims.
  - `TestRunDurationHistogramIsCumulative`: bucket counts are non-decreasing, `+Inf == count`, and `sum` equals the observed total.
  - `TestOldestRunnableAge`
  - `TestRenewalsAreCounted`: a worker with RenewEvery 10ms and a manual clock, with the run held over several renewals, gives renewals ≥ 2.
  - `TestExpositionIsWellFormed`: a strict line parser in the test accepts only `# HELP`, `# TYPE`, and `name{labels} value` lines, and each family's TYPE comes before its samples.
- [ ] **Step 2:** Run and confirm FAIL. **Step 3:** Implement. **Step 4:** `go test -count=1 ./internal/metrics/ ./internal/worker/ ./cmd/twinwright/` gives PASS. **Step 5:** Commit.

### Task 11: The observability stack, proven in CI, and ADR 0032

**Files:**
- Modify: `deploy/otel-collector.yaml` (`otlp` exporter to `jaeger:4317` with TLS insecure, plus `debug`; `health_check` extension on 13133), `docker-compose.yml` (services `jaeger`: `jaegertracing/jaeger` pinned 2.x tag, ports 127.0.0.1:16686 and internal 4317; `prometheus`: `prom/prometheus` pinned tag, 127.0.0.1:9090, `extra_hosts: host.docker.internal:host-gateway`, config `deploy/prometheus.yml` scraping `host.docker.internal:9095` every 2s)
- Create: `deploy/prometheus.yml`, `internal/trace/collector_test.go`, `internal/metrics/prometheus_test.go`, `docs/adr/0032-observability-verified.md`
- Modify: `.github/workflows/integration.yml` (job `observability`), `CHANGELOG.md`

**Behaviour:**
- `TestSpansAreQueryableInJaeger` is gated on `TWINWRIGHT_TEST_OTEL=1`, with `TW_OTLP_ENDPOINT` (default `http://127.0.0.1:4318`) and `TW_JAEGER_URL` (default `http://127.0.0.1:16686`). It:
  - builds a scripted company-incident run on SQLite;
  - exports it through `trace.Exporter`;
  - polls `GET {jaeger}/api/traces/{traceID}` for up to 60s;
  - asserts the span count equals the local trace's, the root span's operation name, and that some span has tag `twinwright.event.start_seq`.
- `TestPrometheusScrapesAWorker` is gated the same way, with `TW_PROMETHEUS_URL` (default `http://127.0.0.1:9090`). It:
  - serves `metrics.Serve` on `0.0.0.0:9095` over a store with one queued run;
  - polls `GET {prom}/api/v1/query?query=twinwright_queue_entries` for up to 60s;
  - asserts a sample with `state="runnable"` and value 1, and that `twinwright_worker_claims_total` exists.
- The CI job runs `docker compose up -d otel-collector jaeger prometheus`, waits on the collector health endpoint and Jaeger's UI port, then runs both tests with `-v`. On failure it prints `docker compose logs`.
- The ADR records what is proven (ingestion and query by trace ID; a real scrape and query), the pinned versions, and the limits: no TLS or auth on the stack, local loopback only.
- [ ] **Step 1:** Write both gated tests, and confirm they skip locally.
- [ ] **Step 2:** Write the configs and the job, and validate the YAML with `docker compose config` if Docker is available. If not, say the job's first CI run is the evidence.
- [ ] **Step 3:** ADR and CHANGELOG. **Step 4:** `go test -count=1 ./...` gives PASS. **Step 5:** Commit.

## Part D: Bring-your-own agent

### Task 12: The `exec` agent protocol and a Python reference agent

**Files:**
- Create: `internal/agent/exec.go`, `internal/agent/exec_test.go`, `docs/protocol/agent-v1.md`, `docs/protocol/agent-v1-request.schema.json`, `docs/protocol/agent-v1-response.schema.json`, `examples/agents/python/billing_agent.py`, `examples/agents/python/README.md`
- Modify: `cmd/twinwright/providers.go` (provider `exec`; flags `--agent-command`, `--agent-env` repeatable), `internal/agent/provider.go` (`KnownProvider` includes `exec`), `cmd/twinwright/main.go` (resume and worker require `--agent-command` for exec runs, with an error naming the flag)

**Interfaces:**

```go
const AgentProtocol = "twinwright.agent.v1"
type ExecProvider struct {
    Command []string      // argv; no shell
    Env     []string      // NAME=value pairs passed through; the child also gets PATH and HOME only
    Timeout time.Duration // per turn; default DefaultCallTimeout
    Wrap    func(argv []string) []string // Task 13 container wrapper; nil = run directly
}
// Request line (stdin, one JSON object, then EOF):
//   {"protocol":"twinwright.agent.v1","task":string,"history":[Message...],"operations":[{"operation_id","method","path","description","parameters"}]}
// Response (stdout, one JSON object): {"content":string,"tool_calls":[{"id","operation_id","arguments"}],"usage":{...}?}
const MaxAgentOutputBytes = 1 << 20
```

Behaviour:
- The agent process is started once per turn.
- stdout is limited to `MaxAgentOutputBytes`; anything over is an error.
- stderr is kept to its last 4 KiB, redacted with `redact.String` over the passed env values, and included in errors.
- A non-zero exit, invalid JSON, an unknown field, or a `protocol` mismatch in the response is an ordinary provider error.
- A timeout kills the process group and returns `CallInterrupted{Kind: InterruptTimeout}`.
- The run's model is `--model`, which is required for exec and names the agent and its version.
- The reference agent solves duplicate-charge deterministically with the standard library only, and has an optional `--echo-request` debug flag that writes to stderr.

- [ ] **Step 1: Write failing tests:**
  - `TestExecProviderCompletesTheBillingRun`: it uses a Go helper process (re-exec of the test binary through `TestMain`, env `TW_EXEC_AGENT_HELPER`) that speaks the protocol. The run passes evaluation and `replay.Verify` verifies.
  - `TestExecProviderRejectsMisbehavingAgents`: subtests for non-JSON, exit 3 (whose error contains the stderr tail), hang (the timeout fires within 2s with Timeout 500ms and the process is gone), stdout flood, and an unknown response field.
  - `TestExecChildEnvironmentIsMinimal`: the helper reports its environ, and it contains only PATH, HOME and the passed names. A test-set `OPENAI_API_KEY` is absent.
  - `TestPythonReferenceAgent`: skipped without `python3`. It runs `examples/agents/python/billing_agent.py` through exec and the evaluation passes.
  - `TestResumeOfExecRunNeedsTheCommand` (cmd).
- [ ] **Step 2:** Run and confirm FAIL. **Step 3:** Implement. The schemas must validate the helper's request and response examples in a test that decodes the schema files as JSON and checks their `required` lists against the struct fields. **Step 4:** `go test -count=1 ./internal/agent/ ./cmd/twinwright/` gives PASS. **Step 5:** Commit.

### Task 13: Container isolation for agents, and ADR 0033

**Files:**
- Create: `internal/agent/execcontainer.go`, `internal/agent/execcontainer_test.go`, `internal/agent/execcontainer_integration_test.go`, `docs/adr/0033-agent-protocol-and-isolation.md`
- Modify: `cmd/twinwright/providers.go` (flags `--agent-container IMAGE`, `--agent-network` default `none`, `--agent-memory` default `512m`, `--agent-cpus` default `1`, `--agent-pids` default `128`, `--agent-mount DIR`, which is mounted read-only at `/agent`), `.github/workflows/integration.yml` (the docker job runs the integration test and pulls `python:3.12-alpine`), `docs/security-review.md` (an "agent processes" section with tests cited; keep `security_review_test.go` green), `CHANGELOG.md`

**Interfaces:**

```go
type ContainerOptions struct { Image, Network, Memory, CPUs string; PIDs int; Mount string }
func (o ContainerOptions) Validate() error // reuse internal/container's memory/cpu/network patterns; Mount must be an existing absolute directory; Image non-empty
func (o ContainerOptions) Wrap(argv []string) []string
```

`Wrap` returns exactly:

```
docker run --rm -i --read-only --cap-drop ALL --security-opt no-new-privileges --user 65534:65534 --network <net> --memory <m> --cpus <c> --pids-limit <p> --tmpfs /tmp:rw,size=16m [-v <mount>:/agent:ro] <image> <argv...>
```

- [ ] **Step 1: Write failing tests:**
  - `TestContainerWrapIsHardened`: the exact argv for a sample config.
  - `TestContainerOptionsRejectBadValues`: `--memory lots`, `--network host`, a relative mount and an empty image are each refused, with `host` refused by name because it removes isolation.
  - `TestPythonAgentInHardenedContainer`: gated on `TWINWRIGHT_TEST_DOCKER=1` and `python:3.12-alpine`. It runs the reference agent with `--agent-mount examples/agents/python`, the billing run passes, and a second subtest proves `--network none` by an agent variant that tries an outbound connection and fails.
- [ ] **Step 2:** Run and confirm FAIL, and that the gated test skips. **Step 3:** Implement. The ADR states the trust boundary: the agent never receives the DSN or provider keys and can only propose calls the dispatcher authorizes. It also states the limits: the Docker daemon is trusted; there is no custom seccomp profile; an allowed network is unrestricted; and on macOS the boundary is the Docker Desktop VM. **Step 4:** `go test -count=1 ./...` gives PASS. **Step 5:** Commit.

## Part E: Reproducible benchmarks

### Task 14: `twinwright perf`

**Files:**
- Create: `internal/perf/perf.go`, `internal/perf/workloads.go`, `internal/perf/perf_test.go`, `cmd/twinwright/perf.go`
- Modify: `cmd/twinwright/main.go` (dispatch `perf`; usage string), `Makefile` (`perf` target)

**Interfaces:**

```go
type Config struct {
    DSN        string        `json:"-"` // SQLite file path or postgres:// DSN; empty = temp SQLite file
    Workers    []int         // default 1,2,4,8
    Runs       int           // runs per throughput trial, default 40
    Trials     int           // default 3
    LeaseTTLs  []time.Duration // recovery workload, default 1s,2s,5s
    ReplayRuns int           // default 20
    Seed       int64         // default 42
    Workloads  []string      // subset of throughput, recovery, replay, crash-matrix; default all
}
type Stat struct { Min, Median, Max float64 } // over trials
type Report struct {
    SchemaVersion int `json:"schema_version"` // 1
    Environment   bench.Environment `json:"environment"`
    Backend       string `json:"backend"`   // sqlite | postgres; never the DSN
    Config        Config `json:"config"`    // DSN omitted from JSON
    Throughput    []ThroughputResult `json:"throughput,omitempty"` // {workers, runs, runs_per_second Stat, wall_ms Stat}
    Recovery      []RecoveryResult   `json:"recovery,omitempty"`   // {lease_ttl_ms, recovery_ms Stat, duplicate_mutations int}
    Replay        *ReplayResult      `json:"replay,omitempty"`     // {runs, verified, ms Stat}
    CrashMatrix   *MatrixResult      `json:"crash_matrix,omitempty"` // {scenario, cases, violations, wall_ms}
}
func Run(ctx context.Context, cfg Config) (Report, error)
```

Workloads (scripted duplicate-charge, fixed seed):
- **throughput:** enqueue `Runs` runs, start W in-process `worker.Worker`s on a real clock with a fenced runner, the same wiring as `workerCommand`, and time until the queue drains; repeat `Trials` times.
- **recovery:** use `crashmatrix.CrashWith` at `after-tool-commit` on `createRefund` with a real clock and the given TTL. Measure from the crash until a polling worker (poll every 100ms) reports `Finished`, and count duplicate mutations (must be 0).
- **replay:** `replay.Verify` over `ReplayRuns` completed runs, timing each.
- **crash-matrix:** `crashmatrix.Enumerate` plus `RunCase` over every duplicate-charge case, giving violations and wall time.

The CLI is `twinwright perf [--db DSN] [--workers 1,2,4] [--runs N] [--trials N] [--workloads ...] --out perf.json`. It writes the JSON and prints a short text summary. It exits non-zero if any recovery case duplicated a mutation or the crash matrix reported a violation.

- [ ] **Step 1: Write failing tests:**
  - `TestPerfReportShape`: `Workers: [1,2]`, `Runs: 4`, `Trials: 1`, `LeaseTTLs: [1s]`, `ReplayRuns: 2` on temp SQLite. Every section is present, `runs_per_second.Min > 0`, `duplicate_mutations == 0`, `replay.verified == 2`, `crash_matrix.violations == 0`, `backend == "sqlite"`, and the JSON contains no `dsn` key.
  - `TestPerfRecoveryIsBoundedByTheLease`: TTL 1s gives a recovery median between 1000ms and 3000ms.
  - `TestPerfCLIWritesTheReport`
- [ ] **Step 2:** Run and confirm FAIL. **Step 3:** Implement. **Step 4:** `go test -count=1 ./internal/perf/ ./cmd/twinwright/` gives PASS. **Step 5:** Commit.

### Task 15: Measured results, methodology, and ADR 0034

**Files:**
- Create: `docs/benchmarks/2026-10-27-m1-mac-sqlite.json`, `docs/benchmarks/2026-10-27-github-postgres17.json`, `docs/adr/0034-benchmark-methodology.md`
- Modify: `docs/performance.md` (new sections "Platform benchmarks", "Methodology", "Results", "Not measured"), `.github/workflows/integration.yml` (job `perf`, `workflow_dispatch` only, with a postgres:17 service; runs `twinwright perf --db "$DSN" --out perf.json` and uploads it), `CHANGELOG.md`

**Behaviour:**
- The controller runs `twinwright perf --trials 3 --out …` on the development Mac and dispatches the CI `perf` job, then hands both JSON paths to this task's implementer.
- The implementer copies the JSON files in unchanged and writes the results tables only from those files. Every figure in the doc must appear in a committed JSON file.
- The doc names the machine, OS, Go version, commit, backend and trials for each.
- It states that GitHub runner figures are from a shared machine, and that recovery latency is in-process with a real clock.
- Unfavourable results are reported as they are: for example, SQLite throughput not scaling with workers because of the single connection.
- [ ] **Step 1:** Write a test in `cmd/twinwright/benchdocs_test.go`, `TestBenchmarkDocsMatchTheirReports`. It parses each committed `docs/benchmarks/*.json` as a `perf.Report` and asserts that every `runs_per_second` median quoted in `docs/performance.md`'s Results table appears in a report. Confirm it fails before the doc exists.
- [ ] **Step 2:** Write the docs and the ADR from the provided JSON. **Step 3:** `go test -count=1 ./...` gives PASS. **Step 4:** Commit.

## Part F: Twinwright Cloud packaging

### Task 16: One-command demos

**Files:**
- Create: `scripts/demo-stack.sh`
- Modify: `Makefile` (`demo` runs `twinwright demo --dir` into a temp dir; `demo-stack` runs the script), `docker-compose.yml` (if a worker image is needed, prefer running workers on the host), `.github/workflows/integration.yml` (job `demo-stack` runs `make demo-stack`)

**Behaviour:** `scripts/demo-stack.sh`:
- runs `docker compose up -d` (postgres, otel-collector, jaeger, prometheus) and waits for health;
- builds the CLI and the company manifest;
- enqueues 6 company-incident runs and 2 duplicate-charge runs;
- starts 2 workers with `--metrics-addr` on ports 9095 and 9096 (prometheus.yml scrapes both) and `--otlp-endpoint`, with `--drain`;
- kills one worker after its first claim to show a takeover;
- queries Jaeger for each completed run's trace ID and Prometheus for `twinwright_worker_takeovers_total`;
- gates the bench against the baseline;
- prints an evidence summary and the URLs (Jaeger UI, Prometheus);
- exits non-zero when any evidence is missing.

It leaves the stack running and prints `make down` to stop it.

- [ ] **Step 1:** Write the script. Add it to the Integration workflow, and validate with `bash -n` and `shellcheck` if available.
- [ ] **Step 2:** `make demo` gives 12/12 locally. The `demo-stack` CI job is its evidence, because the development Mac has no Docker.
- [ ] **Step 3:** Commit.

### Task 17: Documentation as Twinwright Cloud

**Files:**
- Create: `docs/architecture.md`, `docs/claims.md`
- Modify: `README.md` (rewrite), `SECURITY.md`, `CHANGELOG.md` (tidy Unreleased), `docs/distributed.md` (the parked milestone 1 items: the Ctrl-C release is bounded at 5 seconds and skipped on a second Ctrl-C, after which the lease lapses within its TTL; the resume hint)

**Behaviour:**
- **README:** under 450 lines, titled "Twinwright Cloud". Sections: what it is; a 60-second quick start (`make demo`, `make demo-stack`); the regression gate in CI; running your own agent; real models and cassettes; the fleet; observability; benchmarks (linking `docs/performance.md`); limitations; development. Detail moves to `docs/`, with every removed section's content reachable from a link.
- **docs/claims.md:** a table with one row per success criterion of the spec and per README claim: claim | proven by (test name or CI job) | where it runs (local, CI fast lane, Integration).
- **docs/architecture.md:** the component diagram (mermaid), the request path of one tool call, the trust boundaries (agent process, provider, database, observability stack), and the ADR index.
- [ ] **Step 1:** Write `TestClaimsTableReferencesExist` in `cmd/twinwright/claims_test.go`. Every backticked `Test…` name in `docs/claims.md` must exist as `func TestX(` in a `_test.go` file, and every CI job named must exist in `.github/workflows/*.yml`. Confirm it fails before the file exists.
- [ ] **Step 2:** Write the docs. **Step 3:** Run the full gate: `make check && go test -count=1 ./... && go test -race -count=1 ./... && ./scripts/e2e.sh && make bench-smoke && make demo && go mod tidy && git diff --exit-code go.mod go.sum`. Expected: all exit 0. **Step 4:** Commit.
