# Changelog

Notable changes, newest first. Dates are the merge date. Versioning follows
[Semantic Versioning](https://semver.org); while the major version is 0 the
runtime schema and CLI surface may still change, and each entry says when they
do.

The database schema is versioned separately and recorded in `schema_migrations`.
`twinwright version` reports the schema version a build writes. Opening a
database written by a newer build is refused rather than downgraded.

## Unreleased

### Fixed

- **On PostgreSQL, two workers could both take a never-leased run at fence 1, and ownership
  writes could collide.** A run with no lease row has nothing to lock, so concurrent first
  acquisitions both read "no lease" and the later one overwrote the earlier one's live lease
  without `ErrLeaseHeld`: in the new test eight workers all won. Acquisition now creates an
  unowned row first and re-reads it under the lock, so exactly one wins
  (`TestConcurrentFirstAcquireYieldsOneHolderPostgres`). The ownership log picked
  `MAX(seq)+1` with no lock, so a stale worker recording a fence rejection collided with a
  legitimate owner's write on the primary key and one of them failed (78 failures in the new
  test); appends now take the run's lease row first
  (`TestConcurrentOwnershipAppendsNeverCollidePostgres`). A release and its log row now commit
  in one transaction, so a failed log write no longer reports "lease not released" for a lease
  that was released (`TestReleaseAndItsOwnershipRowCommitTogether`).
- **A lease renewal that fails no longer abandons a healthy run, and a one second lease no
  longer fences its own worker.** Any renewal error was treated as a lost lease, so a
  database blip cancelled the run, reported the claim as fenced and wrote a false
  `fence.rejected` row whose reason was `context canceled`. Only a refusal by the store
  does that now. Other errors are logged and retried at the next interval while the lease is
  valid by the worker's own clock, the run is abandoned once it is not, and a fence rejection
  is recorded only for a real refusal, with the store's reason
  (`TestTransientRenewalErrorDoesNotAbandonTheRun`,
  `TestRenewalErrorsPastTheLocalExpiryAbandonTheRun`,
  `TestAbandonedRunRecordsTheRenewalCause`). The renewal interval was floored at one second,
  so with the store's minimum lease of one second the first renewal ran at expiry and was
  refused, and every run longer than the lease failed. It is now a third of the TTL with a
  100 ms floor that cannot reach the TTL, and an explicit interval at or above the TTL is an
  error (`TestOneSecondLeaseRenewsAtLeastTwiceDuringALongerRun`,
  `TestRenewalCadenceLeavesMarginAtTheMinimumTTL`, `TestRenewEveryAtOrAboveTheTTLIsRefused`).
- **The runner's own writes now have a test that a superseded fence is refused.** The
  model request, the model turn (which overwrites the transcript and step), a provider
  failure and the completed, paused and failed statuses are fenced in code, but only the
  dispatcher's writes had a stale-worker test: removing the fence from all seven runner
  calls passed every suite. A stalled runner whose lease was taken over is now shown unable
  to rewrite the finished run (`TestStaleRunnerCannotCommitAModelTurnAfterTakeover`), each
  of the seven writes is reached on its own (`TestStaleRunnerWritesNothingAfterTakeover`),
  and the crash matrix has the dead worker attempt a model turn and a status change in
  every case. No behaviour changed.
- **Commands that take `--db` stopped printing the PostgreSQL password when it holds an
  unencoded `/`, `#` or `?`.** The `perf` fix did not reach the store's own open, so
  `run`, `worker`, `queue`, `trace` and the rest printed `url.Parse`'s quote of the
  password, or, for a DSN that parsed into the wrong pieces, the rest of it inside pgx's
  connect error. A DSN that does not parse cleanly is now refused before anything parses or
  uses it, with a message that says to percent-encode the password and quotes no part of the
  DSN. Both of the store's DSN opens do this, and every command opens its database through
  one of them; the CLI test runs `enqueue`, `queue` and `doctor`
  (`TestOpeningAMalformedDSNNeverPrintsIt`, `TestStorageCommandsNeverPrintAMalformedDSN`).
- **`inspect`, `queue` and `doctor` no longer migrate the database or create one.** They
  opened the source for writing, so a version 2 database came out at version 3 (the
  migration ADR 0029 says must wait until older workers stop), and a mistyped `--db` left
  a new migrated file behind; `queue` on that path exited 0 with an empty queue and
  `doctor` reported the version it had just written. All three now open the database
  read-only, as `trace`, `replay` and `evaluate` already did. A missing path is an error that
  names it, `doctor` reports the version the database had and says when a migration is
  pending, and a database too old for the command is refused with a request to run a
  command that migrates it. The file comes out byte for byte unchanged
  (`TestReadCommandsDoNotMigrateTheSource`, `TestReadCommandsRefuseAMissingDatabase`,
  `TestDoctorReportsTheAppliedSchemaVersionWithoutMigrating`,
  `TestReadCommandsAskForAMigrationWhenTheSchemaIsTooOld`, `TestRequireSchema`). The
  documented `twinwright queue --dsn` is `--db`.
- **`trace --otlp-endpoint` no longer prints the endpoint's credentials.** The delivery
  report named the endpoint as given and the error for a refusal or a failed post quoted
  it, so a password in the userinfo or a token in the query went to stdout and stderr,
  though the worker had always scrubbed the same thing from its log. The exporter now
  names the address alone in `Delivery.Endpoint` and in every error, and masks header
  values and the endpoint's own credentials if a collector echoes them back
  (`TestTraceCommandExportDoesNotPrintEndpointCredentials`,
  `TestExporterNeverReturnsTheEndpointCredentials`). An endpoint with a query string was
  posted to the wrong path, with the traces path appended inside the query, and now keeps
  its query and gets the path added to its path (`TestExporterKeepsTheQueryOfTheEndpoint`).
- **Collector headers can come from `OTEL_EXPORTER_OTLP_HEADERS`.** `trace` and `worker`
  read the standard variable (comma-separated `name=value` pairs, values percent-decoded)
  when `--otlp-header` is not given, so a long-running worker's credential need not sit in
  its command line; the flag still works and wins. A malformed variable is refused before the
  database is opened, without repeating a value (`TestResolveOTLPHeaders`,
  `TestWorkerReadsTheOTLPHeaderFromTheEnvironment`).
- **`perf` no longer prints the PostgreSQL password when it holds an unencoded `/`, `#` or
  `?`.** `url.Parse` rejects such a DSN and quotes a piece of the password in its error,
  which the scrubber's search for the whole DSN never found, so `perf --db` printed it
  (the `/` case printed the complete password). An error about a PostgreSQL DSN that does
  not parse cleanly now has its text replaced by a fixed message, and the two tests that
  cited the guarantee used a DSN whose error never held the password, so they passed
  without the scrubber; they now use malformed DSNs and fail without it
  (`TestRunNeverPrintsAMalformedDSN`, `TestPerfCLINeverPrintsTheDSN`).
- **A base URL that does not parse no longer puts its credentials into errors, the ledger
  or a trace.** The three HTTP adapters redacted only the error from sending a request.
  The error from building one quoted the whole URL, so an unencoded `%` in a password, a
  trailing newline or a bad port printed the userinfo and query on stderr, stored them in
  the run's `error` event and copied them into the model span. Both errors now go through
  one redaction that drops `url.Parse`'s own text, and `run` refuses an invalid `--base-url`
  before it creates a run (`TestMalformedBaseURLNeverLeaks`, `TestBuildNeverCarriesAMalformedBaseURL`,
  `TestMalformedBaseURLNeverLeaksFromTheCLI`). `redact.URL` now fails closed on a string it
  cannot parse: it trims whitespace, drops the fragment and keeps only the scheme and host
  when the string holds no `@`, and `[unparseable URL]` otherwise, where it used to keep the
  userinfo of a string with a leading space or a `/`, `#` or `?` in the password
  (`TestURLFailsClosedOnAnUnparseableURL`).
- **An OpenAI-compatible base URL with a query keeps it.** `/chat/completions` was appended
  after the query, so `http://host/v1?api-version=1` was sent to `/v1`
  (`TestChatBaseURLQueryIsKept`).
- **The configured key is masked in more places.** The success-path mask now also covers a
  key the server escaped in its JSON (`\u0026`, `\/`), the response's `id`, `model` and stop
  reason, and a key in the URL path of an error or a cassette (`TestSuccessfulOutputIsRedactedWhenEscaped`,
  `TestEchoedTopLevelFieldsAreMasked`, `TestKeyInTheURLPathIsMasked`).
- **A cassette replays a redirect, and is always mode 0600.** A 3xx exchange now records its
  `Location` (without userinfo, query or fragment) and the replay serves it, where it used to
  serve a redirect the client could not follow and fail with `HTTP 308` on a run that had
  succeeded live (`TestCassetteReplaysARedirectedExchange`). `--record-cassette` over an
  existing 0644 file left it 0644; the mode is now set whether the file is new or not
  (`TestRecordingTransportReplacesWithMode0600`).
- **An Anthropic 529 is retried once.** Anthropic documents 529 as `overloaded_error`, a
  temporary overload, yet the Anthropic adapter failed the run on it at the first attempt,
  while the same overload reported as a 503 was retried. It is now a `server_error`
  interruption for that adapter only, and the comment that stated why other 5xx codes are
  excluded says so (`TestAnthropicOverloadedIsRetriedOnce`). ADR 0031's claim about a recorded
  timeout was corrected (it held only for a deadline that passes before the response
  headers) and the mid-body failure it describes now has a test at the adapter
  (`TestBodyFailurePartWayIsNotRetried`, `TestRecordedBodyFailureReplaysAsAnOrdinaryError`).
- **The live provider tests no longer print the key when it leaked.** The check that finds the
  configured key in a turn printed the whole turn with `%+v` on failure, so the failure
  wrote the key into test output that is routinely pasted into issues. A failure now prints the problem,
  the names of the fields that held the key and the turn's metadata with the key masked
  (`TestLiveFailureLineNeverCarriesTheKey`).
- **`scripts/live-model.sh` no longer prints the base URL, and its key check is exact.** The
  note about a missing model digest printed the base URL with its userinfo and query, on
  every run against a server without `/api/tags`; it now names the host and port, as the
  report does. The check that the value of `OPENAI_API_KEY` is in no output file also
  searched only some of them and failed every clean run for a key under 8 characters, such
  as Ollama's placeholder `ollama`, which is in every response as `fp_ollama` and which the
  masks ignore on purpose. It now includes the database and its write-ahead log and skips
  a short key with a message (`TestLiveModelScriptNeverPrintsTheBaseURL`,
  `TestLiveModelScriptSkipsTheKeyCheckForAKeyTheMasksIgnore`,
  `TestLiveModelScriptChecksTheDatabaseForTheKey`).
- **A passed value no longer reaches the ledger through an exec agent's stderr.** The error
  for an agent that failed quoted the end of its stderr and masked it afterwards, and
  quoting rewrites a quote, a backslash or a newline, so a `--agent-env` value holding one
  was no longer the text the mask looked for; a value that the 4 KiB window cut in two kept
  its end. The tail is now masked before it is quoted and before it is cut, in the value's
  raw, JSON-escaped and Go-quoted spellings, and starts at a line boundary
  (`TestExecStderrMasksValuesWithEscapedCharacters`, `TestExecStderrMasksAValueCutByTheTail`,
  `TestExecStderrSecretsNeverReachTheLedger`, `TestTailBufferNeverShowsTheEndOfAValueItCutOff`).
- **An exec agent started with no `PATH` and no `HOME` no longer inherits Twinwright's
  environment.** With nothing to pass, the child environment was a nil slice, which
  `os/exec` reads as "use mine", so every provider key and database address went to the
  agent. It is now an empty environment (`TestExecChildEnvironmentStaysEmptyWithoutPathAndHome`).
- **A process an exec agent started no longer outlives the turn.** The agent's process
  group was signalled only on a timeout or an output overflow, so a helper left running by
  a turn that succeeded, or by one that failed because it held the output open, kept
  running, and the guide's "nothing survives between turns" was false. Every turn now ends
  by killing the group on unix (`TestExecProviderKillsTheGroupAfterACleanExit`,
  `TestExecProviderKillsTheGroupOnWaitDelay`). On Linux the agent is also killed if
  Twinwright dies mid-turn, where a hung agent used to be left running for ever
  (`TestExecAgentDiesWithTheProcessThatStartedIt`); this covers the direct child only, and
  a process that left the group, grandchildren of a killed Twinwright and other platforms
  are not covered, which the guide and ADR 0033 now say.
- **A passed value no longer breaks or rewrites an exec agent's answer.** The agent's
  stdout bytes were masked before they were decoded, so a `--agent-env` value that was the
  digits of a number made a valid response fail as malformed JSON, and a value in an
  argument rewrote the call; a value the agent encoded its own way (Python's `\u00e4`, a raw
  `&`) was not recognised at all. The response is now decoded first and only the decoded
  strings are masked: content, ids, and argument names and values at any depth. A value used
  as an argument is masked before the call is dispatched, so the ledger and the tool receive
  `[REDACTED]`, which the guide says; values under 8 characters are never masked, which it
  also says (`TestExecMaskingKeepsTheResponseValid`, `TestExecMasksEveryStringOfTheDecodedResponse`,
  `TestExecMasksValuesHoweverTheAgentEncodedThem`, `TestExecMasksWhatPythonEncodes`,
  `TestExecMaskedArgumentsAreWhatTheLedgerAndTheToolSee`).
- **A `usage` that breaks the schema is refused, not recorded as zeros.** The response
  schema says `usage`, when present, is an object with `input_tokens`, `output_tokens` and
  `total_tokens`, but the decoder accepted `{}`, an object with some of them and `null`, and
  recorded zeros or nothing for a turn's cost. It now refuses all three, and an unknown member,
  a count that is not an integer and a non-object (`TestExecProviderRejectsMisbehavingAgents`,
  `TestExecUsageIsHeldToTheSchema`).
- **An agent that cannot be started no longer puts its command in the ledger.** The error for a
  start failure quoted what `os/exec` says, `fork/exec /home/operator/private-agents/agent: no
  such file or directory`, so the path of the command went into the run's `error` event, though
  the guide said the ledger records nothing about how the agent was started. The error now gives
  the operating system's reason alone (`TestExecStartFailureNeverPutsTheCommandInTheLedger`). The
  stderr of an agent that failed after starting is its own text and can still name its script, and
  the guide and ADR 0033 now say that the claim holds for a run that succeeds.
- **The test for the asynchronous stdin write now fails if the write is made synchronous.** The existing
  guard used an agent that exits at once, which closes the pipe, so a synchronous write
  failed with EPIPE instead of blocking and the test passed with the goroutine removed;
  a request larger than a pipe buffer sent to an agent that never reads and never exits,
  which is what the goroutine is for, was in no test. It is now: the turn must end with a
  timeout within its deadline and the agent must be gone
  (`TestExecProviderTimesOutAnAgentThatNeverReadsALargeRequest`, which blocks and fails
  against a build with the synchronous write). `worker` applied
  `--agent-command` to every run it claimed, and the provider refuses those flags for any
  provider but exec, so a worker configured to serve an exec run failed every scripted or
  hosted run in the same queue until it was marked failed. The agent flags now apply to
  runs whose recorded provider is `exec`, in `worker`, `resume`, and the run `fork` and
  `counterfactual` start from; `run`, `bench`, `shadow` and `fork --agent` still refuse them
  for a provider the operator named (`TestWorkerWithAgentFlagsServesAScriptedRun`,
  `TestResumeOfAScriptedRunIgnoresTheAgentFlags`,
  `TestForkAndCounterfactualOnAScriptedRunIgnoreTheAgentFlags`,
  `TestForRunAppliesTheAgentFlagsOnlyToExecRuns`, `TestRunRefusesAgentFlagsForANonExecProvider`).
- **An exec agent's container is removed when its turn is cut short.** On a timeout, a
  cancellation or an output overflow only the docker client was killed, and Docker does not
  stop a container when its client dies, so the container kept running and writing, and the
  guide and ADR 0033 said the process was killed. Each turn's container now has a name of its own
  (`tw-agent-` and 16 random hexadecimal digits) and `docker rm -f` is run for it, bounded
  to 10 seconds and best effort with a logged failure (`TestContainerIsRemovedWhenTheTurnIsCutShort`,
  `TestContainerWrapNamesEachTurnsContainer`, `TestContainerRemovalFailureIsLoggedNotReturned`,
  and against a real daemon `TestHardenedContainerIsGoneAfterOverflowAndTimeout`, whose control
  shows the container still running without the removal). A Twinwright that is killed
  mid-turn still leaves the container running, which the guide and ADR 0033 say.
- **`--agent-memory 0` and `--agent-cpus 0` are refused.** They passed validation and docker
  reads them as no limit (checked on Docker Engine 29.9.2: the cgroup limit stays `max`), so a
  zero removed the memory or CPU cap (`TestContainerOptionsRejectBadValues`).
- **The docker client can reach a daemon configured through the environment, and the container
  never sees it.** The client got only `PATH` and `HOME`, so `DOCKER_HOST`, `DOCKER_CONTEXT`,
  `DOCKER_CONFIG`, `DOCKER_CERT_PATH` and `DOCKER_TLS_VERIFY` did not reach it, and the only
  workaround, `--agent-env DOCKER_HOST`, also put the daemon address in the untrusted
  container. They now go to the client alone, and their values are masked in error text
  (`TestExecContainerClientGetsTheDockerConnectionVariables`,
  `TestContainerWrapNeverCarriesTheDockerConnectionVariables`,
  `TestExecAgentDoesNotGetTheDockerConnectionVariables`).
- **The Docker CI step can no longer pass without running its tests.** A renamed test matched
  nothing, a failed image check skipped, and the network probe accepted any `blocked` answer,
  each leaving a green step. The step now requires the passes by name in the `-v` output, the
  isolation test requires errno 101 (network unreachable), and a bridge-network control shows
  that the same probe connects; the step's shell was run against the real daemon, against a
  skipped run and against a renamed test, and failed on the last two.

### Added

- **Documentation as Twinwright Cloud.** The README is rewritten as the overview of the
  self-hosted platform, in under 450 lines: what it is, a 60-second quick start (`make
  demo` and `make demo-stack`), the regression gate in CI, running your own agent, real
  models and cassettes, the fleet, observability, benchmarks, limitations and
  development. It says plainly that there are no accounts, tenants, web UI or hosted
  service. Every section the README lost moved, word for word apart from the corrections
  noted on each page, into `docs/guide/`, one page per topic, and `docs/guide/README.md`
  maps each old section to its new page. `docs/architecture.md` has a component diagram,
  the path of one tool call traced through the code, the trust boundaries (agent
  process, provider, database, observability stack) and an index of all 34 ADRs.
  `docs/claims.md` is one table of claims with the test or CI job that proves each and
  where it runs, saying `not verified` for what nothing proves and `cut` for scope that
  was dropped. `TestClaimsTableReferencesExist` fails when a row cites a test or a
  workflow job that does not exist, and `TestDocLinksResolve` fails when a relative link
  in the README, the architecture page, the claims table or the guide points at a
  missing file. `SECURITY.md` gained the agent-process, recording and observability
  notes, `docs/distributed.md` gained the bound on the release at the end of a local
  run, the second Ctrl-C and the resume hint (each with the test that holds it, or a
  statement that none does) and the advice to scrape one worker or aggregate with `max`,
  and `CONTRIBUTING.md` describes what the Compose stack now starts and what `make down`
  deletes. No version heading or tag was added: the release decision is the owner's.
- **Two one-command demos: `make demo` and `make demo-stack`.** `make demo` builds the
  CLI and runs the 12-step scripted `twinwright demo` in a temporary directory, with no
  network and no key; the CI end-to-end job now runs the walkthrough through it.
  `make demo-stack` runs `scripts/demo-stack.sh`. It starts the Compose PostgreSQL,
  OpenTelemetry Collector, Jaeger and Prometheus, recreates two databases of its own
  (`twinwright_demo_company` and `twinwright_demo_billing`, never `twinwright`), queues
  six company-incident runs and two duplicate-charge runs, and runs three workers on the
  host with `--otlp-endpoint`. Worker A is killed with SIGKILL while it holds a run and
  worker B takes the run over after the 5 second lease lapses; a PostgreSQL `SHARE` lock
  on the ledger table holds A's run in place, so the kill does not race a run that takes
  milliseconds. The script exits non-zero unless six checks hold: all 8 runs completed;
  the ownership log shows B taking over A's lease; Prometheus counts the takeover in
  `twinwright_worker_takeovers_total`; Prometheus scraped both workers (A's target is
  down after its kill, which is expected); every run's trace is queryable in Jaeger by its
  trace ID; and the benchmark gate passes against the approved baseline. In three local
  runs on WSL2 with Docker (a fresh stack, a re-run against the running stack, and a
  re-run against a Prometheus still holding the old one-target configuration) all six
  checks held; a copy pointed at a missing baseline failed check 6 and exited 1, and a
  SIGTERM during the lock phase left no worker process and no lock session behind. The
  stack is left running and `make down` stops it. While it runs, the two metrics
  endpoints (ports 9095 and 9096) listen on all interfaces with no authentication,
  because the Prometheus container reaches the host through the bridge gateway.
  `deploy/prometheus.yml` now scrapes both ports. Delivery is still at-least-once: the
  takeover re-executes the run from the ledger, and the demo adds nothing to the ledger
  beyond normal runs. A new Integration job, `demo-stack`, runs `make demo-stack` on
  every Integration trigger. On 2026-10-28 it passed on its first run on GitHub
  Actions, with all six checks: 8 of 8 runs completed, the ownership log showed
  demo-worker-b taking over demo-worker-a's lease,
  `twinwright_worker_takeovers_total{worker="demo-worker-b"}` was 1 in Prometheus,
  both workers were scraped, 8 of 8 traces were queryable in Jaeger, and the
  benchmark gate passed.
- **Measured platform results, a methodology, and ADR 0034.** Two `twinwright perf`
  reports are committed unchanged in `docs/benchmarks/`, one for SQLite on a Windows
  PC under WSL2 and one for PostgreSQL 17 on a GitHub runner, each with three trials.
  `docs/performance.md` gains "Platform benchmarks", "Methodology", "Results" and "Not
  measured" sections that name the machine, operating system, Go version, commit,
  backend and trials of each report and copy its figures without rounding.
  `TestBenchmarkDocsMatchTheirReports` holds the throughput table to the reports it
  cites. One result is unfavourable and is stated as measured: SQLite throughput did
  not scale with workers on the measured machine (two workers were slower than one,
  eight were faster than four), and its cause was not investigated, while PostgreSQL
  throughput rose with workers on the runner. The two reports are not a comparison of
  the backends: the machines and the operating system layers differ. A new `perf` job in
  the Integration workflow, run on demand only, runs `twinwright perf` against a
  PostgreSQL 17 service container and uploads the report with a `runner.txt` that
  names the runner. ADR 0034 records the four workloads' definitions, the trials and
  statistics, what the environment block holds and leaves out, the rule that every
  published figure appears in a committed report, and what is not measured.
- **`twinwright perf` measures the platform workloads and writes a report that says
  what produced it.** It runs the scripted duplicate-charge scenario, the one the
  crash matrix and the benchmark suite run, through four workloads: fleet
  throughput (`--workers 1,2,4,8` in-process workers draining `--runs` queued runs
  under fenced leases), crash recovery (a worker killed just after its first refund
  commits, then the time until a polling worker finishes the run, for each of
  `--lease-ttls`, with the committed refunds beyond one counted), replay
  (`replay.Verify` timed over `--replay-runs` finished runs), and the whole
  duplicate-charge crash matrix. `--trials` repeats the throughput and recovery
  measurements and each is reported as a minimum, median and maximum. The JSON report
  (`--out`, required) carries the backend (`sqlite` or `postgres`, never the DSN),
  the configuration, and an environment block with the build, Go version, OS,
  architecture, CPU count, seed and start time. The command exits 1 after writing the
  report when a recovery duplicated a committed mutation or the crash matrix reported
  a violation, and 2 for bad input such as an empty list item, a non-positive worker
  count or a lease TTL under one second. `make perf` runs it on SQLite into
  `perf.json`, which is git-ignored. A figure in the report is one machine's
  measurement, not a property of the project.
- **A hardened container for an exec agent, and ADR 0033 for the protocol and the
  isolation.** `--agent-container IMAGE` runs the agent command behind `docker run
  --rm -i --read-only --cap-drop ALL --security-opt no-new-privileges --user
  65534:65534` with `--network none` by default, a 16 MiB `/tmp`, and memory, CPU and
  process caps (`--agent-network`, `--agent-memory`, `--agent-cpus`, `--agent-pids`,
  defaults `none`, `512m`, `1`, `128`). `--agent-mount DIR` makes a host directory
  visible read-only at `/agent`, and the variables named with `--agent-env` are
  passed by name, never as values. Options are validated before anything starts:
  `host` networking is refused by name, an image that could be read as a docker
  option is refused, memory, CPU and network values are held to the patterns a
  sidecar config uses (now exported from `internal/container`), and a mount must be
  an absolute existing directory with no colon. `TestContainerWrapIsHardened` asserts
  the whole argv. `TestPythonAgentInHardenedContainer` runs the reference agent
  through it against a real daemon and has an agent in `--network none` try an
  outbound connection; it is skipped without `TWINWRIGHT_TEST_DOCKER=1`, and the
  `docker` job of the Integration workflow now pulls `python:3.12-alpine` and runs
  it. ADR 0033 records the protocol, the bounds, the trust boundary and the limits:
  the Docker daemon is trusted, there is no custom seccomp profile, an allowed
  network is unrestricted, on macOS and Windows the boundary is the Docker Desktop
  VM, and a container is left running if Twinwright is killed mid-turn (a turn that
  times out, is cancelled or floods its output has its container removed, and each
  container is named `tw-agent-` and 16 hexadecimal digits). `docs/security-review.md` gains a section for agent processes with every test
  cited, and its line about live model runs now says that a local Ollama run is
  recorded in `docs/live-model-run.md` while hosted providers remain unverified.
- **An `exec` agent protocol: your own program makes the decisions.** `--agent exec`
  starts a command of your choice once per turn, writes one JSON request to its
  stdin (the task, the history as the ledger records it, and the operations the run
  exposes), and reads one JSON response from its stdout: tool calls to propose, or a
  final answer. The protocol is `twinwright.agent.v1`, documented in
  `docs/protocol/agent-v1.md` with JSON Schema files for the request and the
  response. The agent proposes and the dispatcher still authorizes and executes, so
  the run's policy does not depend on the agent behaving. The response must carry
  `protocol`, `content` and `tool_calls`; unknown fields, data after the object, a
  non-zero exit and a protocol mismatch are provider errors. stdout is limited to 1
  MiB and the process is killed past it, stderr keeps its last 4 KiB and is redacted
  with the passed values, and a turn that overruns its deadline gets SIGTERM, then
  SIGKILL after 500 ms, to its whole process group and is recorded as a `timeout`
  interruption that the runner retries once. The child environment is only `PATH`,
  `HOME` and the variables named with the repeatable `--agent-env NAME`; a
  `NAME=value` is refused so no value is typed on a command line. `--model` is
  required and names the agent and its version. The command is not stored with a
  run, so `resume` and `worker` need `--agent-command` again and fail naming the flag
  without it. `examples/agents/python/billing_agent.py` is a deterministic,
  standard-library reference agent that solves `duplicate-charge`; it is a protocol
  example, not a model. `TestExecProviderCompletesTheBillingRun` runs a separate
  process through the whole scenario and verifies the replay; the unix process-group
  calls live in build-tagged files so the module still builds for Windows.
- **A regression gate over bench reports, proven able to fail and run in CI.**
  `twinwright gate` compares a candidate report with an approved baseline and exits
  0 for no regressions, 1 for regressions and 2 when it cannot decide. The approved
  baseline is `examples/bench/baselines/standard-scripted.json`. `--model
  fixture-regressed-v1` selects a scripted fixture that investigates and then
  claims the issue is resolved without making the first write, so the gate can be
  shown to fail. The `regression-gate` CI job gates the current build against the
  baseline and requires the gate to exit exactly 1 on the regressed fixture.
  `TestRegressedBenchFailsTheGate` holds the same pair in `go test`, the demo gains
  a twelfth step that shows the gate refusing the regressed fixture, and `make
  gate` runs the first half locally. `run --fail-on-eval` exits non-zero after
  printing the result when the evaluation fails; it is opt-in. See ADR 0030.
- **A documented security review, gated by a test.** `SECURITY.md` was the policy
  and there was no record of anyone having looked: nothing under `docs/` matching
  secur, audit, review or threat, and no ADR. `docs/security-review.md` is the pass
  over seven surfaces - the authorization boundary and its eight deny reasons,
  prompt injection through world content, secret redaction at write time and read
  time, the container surface, the shadow connector's observe-only guarantee, SQL
  construction, and what is deliberately not covered - with the file and the test
  that hold each claim, and with the places where no test holds one written down as
  limits rather than left out.
  `cmd/twinwright/security_review_test.go` requires every cited file and test to
  exist and every surface to be present, and fails outright if it parses nothing,
  so the document fails the build when it drifts.

  **Two claims `SECURITY.md` already made are now tested.** "Provider errors are
  recorded without credentials" is a claim about the ledger and only the provider's
  return values were asserted, so `TestProviderErrorReachesTheLedgerWithoutTheKey`
  now runs a real provider against a server that echoes the configured key into a
  401 and asserts no event payload and no stored transcript carries it. The one
  interpolated SQL identifier is documented as validated first and nothing
  exercised that, so `TestSearchPathSchemaRejectsAnythingButAPlainIdentifier`
  refuses seven hostile `search_path` values with legal controls beside them. A
  third test covers an `env_file` traversal written in the unnormalised form the
  existing cases had already normalised away. See ADR 0028.
- **Per-attempt provider deadlines, and truncated completions that stop being
  decisions.** Every provider built an `http.Client` with a flat, hard-coded,
  uncancellable 90 second timeout, and nothing in the repository read
  `finish_reason`, `stop_reason` or the Responses API's `incomplete_details`. So a
  hung call took 90 seconds off the run and left nothing in the ledger, and a
  completion the provider cut off at its token ceiling was consumed as a complete
  answer: a tool call cut mid-arguments still decodes into a map, and the runtime
  would dispatch an action built from half a serialisation. The deadline is now per
  attempt and carried on the request context, configurable with
  `--provider-timeout` on every command that can reach a provider and refused if
  non-positive. Each provider reads its own surface's truncation field and returns
  before parsing any output item. Either failure is retried exactly once and every
  attempt is recorded as a `provider.interrupted` event carrying the kind, the
  attempt number, the vendor's verbatim reason and the configured deadline; a run
  where every attempt is interrupted fails with the existing `error` event of kind
  `provider`. The record lives on the turn, so a run that recovered still replays:
  a replayed run appends the same event from the recorded turn instead of needing
  the provider to fail again, which is also why the event is in replay's semantic
  list rather than ignored by it. `AnthropicProvider.MaxOutputTokens` makes the
  ceiling a truncated completion hit configurable, since raising it is the
  documented remedy. See ADR 0027.
- **Shadow comparison depth.** The comparison was an exact match on marshalled
  arguments, so a proposed refund of 500 against an observed refund of 5905 on the
  same charge was reported as two unrelated entries, one in each only-list, and
  the reader had to notice they were the same charge. Three classifiers report it
  instead: `argument_divergences` pairs the leftover actions that address the same
  resource with the same operation and names the fields that differ with both
  values; `timing` reports the pairs the two streams sequence differently; and
  `policy` screens each proposed action against a principal policy given with
  `shadow --policy` and lists what it would refuse, with the permission, the deny
  reason and the call number. A refusal stands even when the observed stream
  contains the same action, flagged `also_observed`, because a human with other
  permissions doing something is not evidence that this principal may. Resource
  identity comes from the arguments that name the resource, not from the operation
  name and not from a call's position, both of which stop identifying anything
  once a call site is added. Elapsed time is reported as not compared rather than
  as zero: a local simulation has no wall clock comparable to a recorded stream.
  Shadow mode stays observe-only, the output stays deterministic and sorted, and
  it still declares no winner. See ADR 0026.

  **`authz.Screen`.** The policy decision for one call at a given call number,
  without consuming one. `authz.Decide` is now `Screen` plus the two things only a
  live run supplies, the stored policy and the next call number, so a shadow
  verdict and an enforced one cannot drift apart.
- **Crash points, an exhaustive crash matrix, and a real-process kill.** Recovery
  had been tested by cancelling a context inside a provider call, which is one of
  seven places a worker can die. `internal/crashpoint` names all seven
  (`before-model-request`, `after-model-request`, `after-model-response`,
  `before-tool-commit`, `after-tool-commit`, `before-completion`,
  `after-completion`), `agent.Runner` and `dispatch.Dispatcher` take a hook that is
  nil in production, and `internal/crashmatrix` crashes every occurrence of every
  point, expires the lease, lets a second worker take over, and checks the
  recovery. `TestCrashMatrixSQLite` runs 120 cases over three scenarios
  (`duplicate-charge` 25, `ambiguous-commit` 15, `company-incident` 80) with zero
  violations; `TestCrashMatrixPostgres` runs the first and last occurrence of each
  point when `TWINWRIGHT_TEST_POSTGRES_DSN` is set. The `postgres` job of the
  Integration workflow sets it and runs `go test -count=1 ./...` against PostgreSQL 16
  and 17, so this test ran against a server there, and the job passed on every
  Integration run of this branch on 2026-10-28. `TestRealProcessDeathIsRecovered` (unix)
  SIGKILLs a re-executed test binary at `after-model-request` #2,
  `after-model-response` #3, and `before-tool-commit` and `after-tool-commit` on
  the first `createRefund`, then recovers with a second worker;
  `TestRealProcessDeathIsRecoveredPostgres` covers the two tool-commit points
  against a server, and the same job runs it. `after-completion` is simulated by
  skipping `FinishRun`. See ADR 0029.

  **`bench` distributed cases name a crash boundary.** Distributed mode now runs on
  the crash matrix and accepts `crash_point` and `crash_operation`
  (`crash_operation` only with `before-tool-commit` or `after-tool-commit`;
  `after-completion` only with `crash_at` 1). An integer `crash_at` alone still
  means `after-model-request` at that turn. The standard suite is 21 cases and
  gains `distributed-crash-after-refund-commit`, which crashes right after the
  refund commits, and a comment that claimed a turn crash followed the refund was
  corrected. `TestFlagshipSuiteRecoversEveryCrashCase` runs `flagship.yaml` in
  `go test`.

  **A crash boundary table in `docs/distributed.md`.** For each of the seven
  points, what is durable at death and the test that proves recovery from it.
- **Provider turns record the provider's own account of the response, and
  `--max-output-tokens` sets the ceiling.** Each adapter stores the response ID,
  the model the server says answered, the finish or stop reason and the HTTP
  status on the turn (`response_id`, `server_model`, `finish_reason`,
  `http_status`), so two runs can be compared field for field. All four are
  omitted when empty, so a scripted run's ledger and the committed bench baseline
  are unchanged. `--max-output-tokens` is accepted by every command that can reach
  a provider: 0 keeps each provider's own default (no ceiling sent to an
  OpenAI-compatible server, 4096 for Anthropic, whose API requires one) and a
  negative value is refused. See ADR 0031.
- **Provider cassettes: `run --record-cassette` and `replay --cassette`.** Replay
  from the ledger reproduces a run's own bookkeeping but never runs the adapter
  that produced the turns. `twinwright run ... --record-cassette PATH` appends
  every HTTP exchange of the provider to a JSON-lines file, a header line naming
  the provider, model and output ceiling and then one line per exchange, and
  `twinwright replay <run> --cassette PATH` re-executes the run through the run's
  own adapter, answered from that file, so response parsing, the recorded metadata
  and the interruption classification are in the loop. Replay needs neither
  `--base-url` nor an API key, and the cassette transport never dials
  (`TestCassetteNeverDials`). A request is matched by method, URL path and request
  hash, and a divergence names the exchange and the first field that differs.
  Request headers are never recorded, a URL loses its userinfo and query, and both
  bodies are masked with the configured key; the hash is taken over the bytes as
  sent, so a replay with a placeholder key still matches. The flag exists on `run`
  only and is refused for the scripted provider and with `--enqueue`. Limits: a
  recorded per-attempt timeout replays as a `transport` interruption, so the
  replay reports a divergence; a `Retry-After` given as an HTTP date is clock
  relative and is not verified to replay identically; a response body that is not
  valid UTF-8 is stored as a JSON string; cassettes for forked or resumed runs and
  an Anthropic record and replay are not tested. See ADR 0031.
- **A real model run: `scripts/live-model.sh`, `make live-model` and the
  `real-model` job.** Every other test answers the provider adapters from a script
  or a recording; this runs a small model that was not written for the test.
  `scripts/live-model.sh [BASE_URL] [MODEL]` (Ollama's `http://127.0.0.1:11434/v1`
  and `$TW_LIVE_MODEL` by default) builds the CLI and the billing manifest, runs
  `duplicate-charge` with `--agent openai-compatible --record-cassette` and
  `--steps 12`, prints the evaluation verdict, replays the run from its ledger and
  then from the cassette, and writes `live-model-report.json`: the model, the
  server's host and port, whether the evaluation passed, model turns, tool calls,
  both replay verdicts, the cassette's exchange count, per-step wall times and the
  model's digest from Ollama's `/api/tags`. It exits non-zero when recording fails,
  when the run does not reach `completed` (replay needs a completed run, and the
  report is still written) or when either replay is not verified; the evaluation
  verdict is reported and never gates. The `real-model` job in `integration.yml`
  starts the pinned `ollama/ollama:0.40.11`, pulls `qwen2.5:1.5b`, runs the script,
  stops the container, replays from the cassette with the server gone, and uploads
  the report and the cassette as artifacts. The script ran against that image on a
  developer machine and the job's steps were run in order there. The job passed on
  GitHub Actions on 2026-10-28, on its first run and on every run since: it recorded
  with `qwen2.5:1.5b`, verified the ledger replay and the cassette replay, then
  verified a second cassette replay with the server stopped. The model's evaluation
  verdict is reported there and never gates. The local run of the script is
  recorded in `docs/live-model-run.md`: the evaluation failed and both replays
  verified. See ADR 0031.
- **Opt-in live provider tests.** `TestLiveOpenAIToolCall`
  (`TWINWRIGHT_TEST_LIVE_OPENAI=1`, `OPENAI_API_KEY`, `TW_LIVE_OPENAI_MODEL`) and
  `TestLiveAnthropicToolCall` (`TWINWRIGHT_TEST_LIVE_ANTHROPIC=1`,
  `ANTHROPIC_API_KEY`, `TW_LIVE_ANTHROPIC_MODEL`) each make one real call offering
  `getCustomer` and require a model turn with a response ID, a 200 status and
  a `getCustomer` call carrying a call ID and an `id` argument. Each skips with a
  message naming what is not set. Nothing runs them in CI, and neither had been
  run against a vendor when this was written.
- **OTLP spans tie back to the ledger, and a worker can export the trace of a run
  it finishes.** Every exported span carries `twinwright.event.start_seq` and
  `twinwright.event.end_seq` (integers, the first and last ledger event the span
  covers) and `twinwright.run_id`, so someone reading the collector can name the
  events behind a span without the database. They are added in the OTLP projection
  only, so `trace --format json` keeps its attributes
  (`TestOTLPSpansCarryLedgerSequence`). `trace.TraceIDFor(runID)` is the one
  derivation of the trace ID that `Build`, the fork link and the worker share
  (`TestTraceIDForIsTheIDBuildUses`), and the delivery `trace --otlp-endpoint`
  reports gains `trace_id` (`TestDeliveryReportsTheTraceID`). `twinwright worker`
  gains `--otlp-endpoint` and `--otlp-header`. Unlike `trace`, the worker does not
  default the endpoint from `OTEL_EXPORTER_OTLP_ENDPOINT`, so export is off unless
  asked for. When a claim ends `finished` on a `completed` run, the worker builds
  and exports that run's trace under a 10 second bound, and the `claim finished`
  log line gains `trace_id`. A paused, failed, requeued or fenced claim does not
  export. Export is best effort: a failure is logged at warn level with the trace
  ID, with the endpoint's userinfo, the header value and the configured provider
  keys scrubbed from the text, and never changes the claim's disposition
  (`TestWorkerExportFailureDoesNotFailTheRun`,
  `TestWorkerExportFailureWarnsWithoutLeakingCredentials`). Nothing about the
  worker, the export or the trace ID reaches the event ledger
  (`TestWorkerExportsACompletedRunsTrace` drains the same scripted run with and
  without export and requires the same sequence of event types). Delivery is still
  at-least-once: a run delivered twice can be exported twice, to the same trace
  ID.
- **Worker metrics for run duration, queue wait and worker liveness.** The
  histogram `twinwright_worker_run_duration_seconds` (labelled `worker`, buckets
  0.05 to 120 seconds and `+Inf`) is observed for every claim whatever its
  disposition. `twinwright_queue_oldest_runnable_age_seconds` is how long the
  longest-waiting runnable entry has been available, 0 when nothing is runnable
  (an entry still in backoff does not count), and
  `twinwright_worker_last_seen_age_seconds{worker}` is the time since each
  registered worker last registered, claimed or renewed a lease
  (`TestRunDurationHistogramIsCumulative`, `TestOldestRunnableAge`,
  `TestWorkerLastSeenAge`). The database-derived gauges are served by every worker
  for the database it points at, so with several workers on one database scrape one
  of them or aggregate with `max`, never `sum`. See ADR 0032.
- **An observability stack in Compose, and an `observability` CI job that checks it.**
  `docker-compose.yml` gains `jaeger` (`jaegertracing/jaeger:2.22.0`, UI and query
  API on `127.0.0.1:16686`) and `prometheus` (`prom/prometheus:v3.15.1`, on
  `127.0.0.1:9090`, scraping `host.docker.internal:9095` every two seconds from
  `deploy/prometheus.yml`). The collector keeps `127.0.0.1:4318`, forwards to Jaeger
  as well as printing what it receives, and exposes a health endpoint on
  `127.0.0.1:13133`. `docker compose up -d` now starts all four services; name the
  ones you want to start fewer. Two opt-in tests (`TWINWRIGHT_TEST_OTEL=1`, skipped
  otherwise) use the stack: `TestSpansAreQueryableInJaeger` exports a scripted
  `company-incident` run through the collector and reads it back from Jaeger by
  trace ID, requiring the same span count, a root with the local root's name and a
  span carrying `twinwright.event.start_seq`; `TestPrometheusScrapesAWorker` serves
  a worker's metrics and requires Prometheus to return a `runnable` queue entry
  count of 1 and the worker's claims counter. The comparison and polling logic have
  ordinary tests against canned answers. The `observability` job in
  `integration.yml` starts the three services, waits on the collector's health
  endpoint and Jaeger's UI port, runs the two tests, prints `docker compose logs` on
  failure and removes the stack. The stack has no TLS and no authentication, every
  published port is on loopback, and the gated metrics listener binds `0.0.0.0`
  only in that test. The job passed on GitHub Actions on 2026-10-28: the spans were
  queried from Jaeger by trace ID, and a real Prometheus scrape and query succeeded.
  The two tests had also passed against the stack on a developer machine. See ADR 0032.

### Fixed

- **A provider turn that reuses a call ID now fails the turn.** A call ID already
  used in the run, or repeated within one turn, used to be skipped; it is refused
  with an explicit error. See `TestProviderCallIDReuseFailsTheTurn`.
- **`run` and `resume` execute under a fenced lease.** Without `--enqueue` they ran
  with no fence and could write to a run at the same time as a fleet worker that
  had claimed it. They now hold a lease owned by `local-<host>-<pid>` with a 15
  second TTL, renewed while they run and released on exit including Ctrl-C and
  `SIGTERM`. A run leased by a live fleet worker is refused, naming the owner and
  expiry; after `kill -9` the lease has to expire before `resume` is accepted;
  a failed release is reported rather than swallowed. Command output and exit
  codes are otherwise unchanged.
- **Lease expiry and queue availability now order by time, not by text.** Both are
  compared inside SQL as text, and `time.RFC3339Nano` trims trailing zeros, so
  `12:00:01Z` sorted after `12:00:01.5Z`. Within one second a renewal could revive
  an expired lease, and a claim could miss an expired lease or a run that had
  become available. Times in `run_leases`, `work_queue` and `workers` are now
  written in a fixed-width form with nine fractional digits, and migration 3
  (schema version 3) rewrites existing rows. A stored time that does not parse
  fails the migration, which rolls back, and names the table, column and row.
  Ledger event timestamps are unchanged. **Upgrading:** stop workers running an
  older build before applying migration 3. An older build keeps writing the
  variable-width form, and a database holding both forms misorders times within a
  second.
- **A run that kills its worker is now failed at the delivery limit, and a takeover
  means a displaced lease.** `MaxAttempts` was checked only when `Execute` returned
  an error, and a crash returns nothing, so a poison run was taken over forever.
  A claim past `MaxAttempts` deliveries now fails the run without executing it,
  with an `error` ledger event `{"kind":"delivery_limit","deliveries":N,"max_attempts":M}`
  and the queue entry `failed`. A run that had already completed is finished
  instead. `Outcome.Takeover` was `attempts > 1`, so an ordinary requeue after an
  error counted as a takeover; it is now true only when the claim displaced an
  expired lease. Re-enqueueing a run resets its delivery count, so an operator
  retry of a run that failed at the limit executes instead of being refused
  again. Delivery remains at-least-once. **Known limit:** a run retried after
  hitting the delivery limit can complete but cannot be verified by replay,
  because its ledger records the `delivery_limit` error and an unanswered model
  request. A test pins the behaviour and ADR 0029 lists it under remaining
  limits.
- **A key echoed by a model, and a base URL that carries credentials, no longer
  reach the ledger or an error message.** Error bodies were masked but a
  successful response was not, so a model that repeated the configured key in its
  content or in a tool argument had it written to the ledger and the stored
  transcript. Every adapter now masks the key in each output item before the item
  is interpreted, so the content, the tool arguments and the raw items kept for
  replay all derive from the masked text (`TestSuccessfulOutputIsRedacted`). A
  failed request returned the standard library's error, which prints the full
  request URL with its userinfo and query; transport errors now go through
  `redact.URL` and their cause text is masked
  (`TestTransportErrorHidesURLQueryAndUserinfo`). Only the configured key is
  masked on the success path, and `redact.Secrets` ignores a secret shorter than 8
  characters. See ADR 0031.
- **The metrics document is well formed, and its counters can be alerted on.** The
  counters' `HELP` and `TYPE` lines were all written first and their samples after,
  so no family was contiguous, which the Prometheus text format requires.
  Each family is now `HELP`, `TYPE`, then its samples
  (`TestExpositionIsWellFormed`). A counter's label name was guessed from whichever
  value was passed to it, so any non-empty label became `disposition`; each counter
  now declares its label, and only `twinwright_worker_claim_disposition_total`
  carries `disposition` (`TestCounterLabelNamesAreDeclaredPerMetric`, and
  `TestMetricsDispositionLabelsMatchTheWorkerDispositions` keeps the four
  dispositions equal to the worker's). A counter existed only after its first
  increment, so `rate()` had no earlier sample to start from; every counter now
  renders a zero sample from the first scrape (`TestCountersRenderZeroBeforeFirstUse`).
  `twinwright_worker_lease_renewals_total` could never move, because nothing counted
  a renewal; the worker now counts each successful renewal
  (`TestRenewalsAreCounted`). Label values went through `%q` after their own
  escaping, which doubled backslashes in worker IDs; they are escaped once. Metric
  names, and the labels of `twinwright_queue_entries` and the run-status gauges,
  are unchanged. See ADR 0032.

### Changed

- **The Go directive is 1.27.3 to take the fix for GO-2026-6809.** The advisory
  is in `crypto/x509` and `govulncheck` reports it against go1.27.2, so the
  vulnerability check failed on any build of that toolchain. The `require` lines
  and `go.sum` are unchanged.
- **The published recovery guarantees now match what the tests prove, including
  where ADR 0020 overstated them.** ADR 0029 states exactly what is idempotent: a
  world mutation commits at most once per (run, call ID); model calls are
  at-least-once, so a crash before a recorded response repeats the provider call
  and its cost; a retry with a new call ID is a new action; provider spend, logs
  and metrics are not fenced. It corrects three claims in ADR 0020. "Crash
  recovery at every model turn" was a crash inside the provider call, one boundary
  of seven. The call-ID cache is not what protects a recovered run, because the
  resumed runner dispatches only calls absent from the transcript and the
  transcript entry commits with the effect. And the lease unit tests ran against
  SQLite only, not PostgreSQL. ADR 0020 carries a one-line pointer and its body is
  unchanged. Remaining limits are listed in ADR 0029: `fork`, `counterfactual`, the
  non-distributed `bench` modes and the shadow simulation execute their own runs
  without a fence, the real-process test kills one
  process at a time, and the matrix covers three scenarios. The README's
  distributed semantics and limitations were updated to match.
- **A response over 4 MiB is an error, and a 429, a 5xx or a dropped connection
  is retried once.** The adapters read a body through a silent 4 MiB limit, so a
  longer body reached the decoder cut in half. It is now `ErrResponseTooLarge`,
  which is not retried, and one byte past the limit is read so a body exactly at
  it is accepted (`TestResponseAtTheLimitIsStillAccepted`,
  `TestReadBodyStopsReadingOneBytePastTheLimit`). A 429, a 500, 502, 503 or 504,
  and a request that failed before any response arrived, used to fail the run on
  the first attempt. They are now interruptions of kind `rate_limited`,
  `server_error` or `transport`, retried once like a timeout or a truncated
  completion, and recorded as `provider.interrupted` events that also carry the
  HTTP status and the server's `Retry-After` in milliseconds as `status` and
  `retry_after_ms`. The wait before the second attempt is the server's
  `Retry-After` (whole seconds or an HTTP date) capped at 30 seconds, or one second
  when none was sent or it was unusable; timeouts and truncations still retry at
  once. Cancelling the run during the wait returns the context error with no second
  attempt. Replay never waits: a replayed turn carries its interruptions, and a
  replay through a cassette skips the wait (`TestRetriedRunStillReplays`,
  `TestCassetteReplayDoesNotWaitOutARecordedRateLimit`). Every other non-2xx status,
  501, 505 and 529 included, stays an ordinary error and is not retried. The span
  event `twinwright trace` writes for each interruption keeps the `status` and
  `retry_after_ms` as well as the kind, attempt, reason and deadline
  (`TestBuildKeepsTheStatusAndWaitOfAProviderInterruption`). See ADR 0031 and
  ADR 0032.

## v0.1.0 - 2026-09-30

First tagged release. Everything below shipped before the tag; the sections are
grouped rather than dated individually.

### Added

- **`twinwright demo`.** One command that walks the whole runtime over the
  flagship incident in eleven steps: compile the four-service world, run it under
  chaos, inspect, trace, evaluate assertions, replay, fork and compare, crash a
  worker and watch another take over, fail the same incident on purpose, explain
  that failure with a counterfactual, and refuse a prompt injection and an
  over-privileged request. Each step executes through the CLI's own entry point,
  so it runs the documented interface rather than a parallel copy, and reads its
  evidence back out of the run. The walkthrough stops at the first step that
  cannot produce its evidence, and CI runs it, so a step that stops being true
  fails the build.
- **`event_follows` assertion operator.** Anchors on the first matching event and
  requires a later match, so existence is part of the claim. `event_order`
  forbids an event from happening early and is therefore satisfied when it never
  happens at all, which is the right operator for "do not comment before reading
  the ticket" and the wrong one for "reconciled after the loss".
- **Negative controls in Bench suites.** `expect: fail` declares a case that is
  supposed to fail, and a misspelling is refused rather than defaulted. A control
  that starts passing is reported as an undetected control instead of a green
  case.
- **Flagship distributed bench suite.** `examples/bench/flagship.yaml` crashes the
  first worker at three points of a sixteen-call trajectory on the four-service
  company world and requires a takeover, a duplicate delivery and a refused stale
  commit on each. The three shipped distributed cases all used the single-service
  billing world, so the headline claim had never been measured on the incident the
  README leads with.
- **Unsafe company fixture.** Selected by `--model fixture-unsafe-v1`, as the
  billing world already did. It differs from the safe fixture by one decision,
  whether a lost write response is taken as proof the write landed, which gives
  counterfactual analysis a genuinely failing parent on the flagship scenario.
- **Distributed runtime documentation.** `docs/distributed.md` carries the
  topology, the crash-and-takeover sequence and the lease lifecycle. Every
  database identifier in those diagrams is checked against the migration DDL, so
  a renamed table breaks the diagram that names it. (ADR 0020)
- **Shadow observation connectors.** A `Connector` boundary decodes a recorded
  external format into observations, so shadow evaluation no longer requires
  hand-transforming an export into Twinwright's own shape first. Three
  implementations ship: `file`/`jsonl` (native), `audit_log` (sanitized audit
  export) and `recorded_http` (captured HTTP interactions), selected by a
  config's `source.type`. Decoding is a pure function of bytes, so no connector
  opens a socket or holds a credential. Confinement under the examples root is
  enforced in the loader as well as at config parse time, sources are capped at
  8 MiB, and unknown fields are rejected rather than silently dropped. An attempt
  that did not take effect (a denied audit entry, a 4xx or 5xx response) is not
  counted as an observed action. No connector has been tested against a live
  external system; shadow output carries `live_external: false`. (ADR 0022)

### Fixed

- **Bench reported a safety figure that covered fewer cases than it claimed.**
  A case that errored and measured nothing was counted as a compliance failure in
  every dimension it declared, and the two deliberate negative controls were
  counted as agent failures, which is how the standard suite reported 25% safety
  compliance while no agent had misbehaved. Errored cases and declared controls
  now leave the rate denominators, the report names both counts, and the footer
  states how many cases a percentage covers. (ADR 0025)
- **A rate over an empty denominator printed as `0.0%`.** A suite declaring only
  some dimensions reported `Safety Compliance 0.0%`, which reads as the opposite
  of the truth: the suite contained no safety case. The text report now labels the
  absence, and the JSON keeps the numeric field beside a `measured` map so a
  machine consumer can tell zero-of-zero from zero-of-many. (ADR 0025)
- **The duplicate-effect rate counted refund rows, not the agent's refunds.** That
  assumes the agent is the only writer, so the one case built to exercise a
  concurrent writer was scored as the defect it exists to rule out. The rate now
  counts refunds the agent itself committed, paired from its own requests and
  responses in the ledger; a chaos actor's write is recorded as
  `chaos.actor_mutation` and is not a tool call, so the separation is exact rather
  than heuristic. (ADR 0025)
- **A concurrent partial refund made the flagship fixture stop rather than
  reconcile.** Billing refuses a refund exceeding what a charge still owes, so an
  amount computed from an invoice read earlier is rejected once another writer has
  moved money. The fixture re-reads the charge and refunds the remainder, so the
  customer ends at exactly the charge amount and never above it.
- **A refusal was read as proof the money was already back.** That holds for a
  fully refunded charge and fails for a partly refunded one, leaving a customer
  short while the agent reported the incident resolved, with nothing in its own
  transcript to show it. The refusal now triggers one fresh read and a refund of
  what is still outstanding, and a read that contradicts the writer stops the run
  instead of reporting a resolution.
- **An unbounded stale-read rule poisoned every later read.** `after_calls` with
  no `times` models a replica that never catches up, which is a different fault
  from the single stale observation the test was named for.
- **Shadow comparison is now deterministic.** `Compare` built its matched and
  residual lists by ranging over maps, so Go's randomised iteration order decided
  the order of a report meant to serve as evidence: two comparisons over
  byte-identical input disagreed, which made a report impossible to diff between
  runs or commit as a fixture. Output is sorted by operation and then canonical
  arguments.
- **Superseded ADRs now say so.** ADR 0018 still read as current after ADR 0020
  replaced its design and removed the `twinwright lease` command, so a reader
  landing there first would implement a lease store that no longer exists and
  cannot enforce fencing. ADR 0017 now credits the two ADRs that completed it,
  and the README capability list no longer advertises the distributed runtime as
  deferred two bullets above the ones describing it as shipped.

- **PostgreSQL storage backend.** `store.OpenDSN` accepts a `postgres://` URL or
  a SQLite path. Runtime SQL is written once with `?` placeholders and translated
  to `$N` by a `database/sql` driver wrapping pgx. SQLite remains the default for
  local development, deterministic examples and replay reconstruction.
  (ADR 0019, schema version 1)
- **Versioned migrations.** Schema state lives in `schema_migrations` instead of
  being inferred from `PRAGMA` probes. Each migration commits with its own
  bookkeeping row, so a failure leaves neither a partial schema nor a false
  record of success. Databases written before migration tracking are adopted in
  place.
- **Multi-worker runtime.** `twinwright run --enqueue`, `twinwright worker` and
  `twinwright queue`. Workers claim runs under fenced leases, renew them while
  executing, and take over runs whose lease lapsed. Delivery is at-least-once
  with idempotent handlers and fencing. (ADR 0020, schema version 2)
- **Transactional fencing.** The ownership check runs inside the same
  transaction as the write it protects, and rejects both a superseded fence and
  an expired lease. The monotonic committed-fence record keeps this correct under
  clock skew.
- **Ownership audit log.** `run_ownership_log` records lease acquisition,
  release, takeover, fencing rejections and run completion, separately from the
  run ledger.
- **OTLP delivery.** `twinwright trace --format otlp --otlp-endpoint <url>` posts
  spans to an OpenTelemetry collector and reports its status code. A rejection is
  an error, not a silent success. (ADR 0021)
- **Prometheus metrics.** `twinwright worker --metrics-addr` serves in-process
  worker counters plus ledger-derived gauges. Unauthenticated and off by default.
- **`twinwright version` and `twinwright doctor`.**
- **CI.** `.github/workflows/ci.yml` runs gofmt, vet, build, tests, the race
  detector, a deterministic end-to-end script, `go mod tidy` verification,
  gitleaks and govulncheck. `integration.yml` runs the suite against PostgreSQL
  16 and 17 service containers, the distributed tests under `-race`, and the
  end-to-end script against PostgreSQL.
- **Developer experience.** `Makefile`, `docker-compose.yml` for PostgreSQL and
  an OpenTelemetry collector, `scripts/e2e.sh`, `CONTRIBUTING.md`, `SECURITY.md`,
  issue and pull request templates.

### Fixed

- **A crash between persisting `model.request` and recording `model.response`
  made a run permanently unreplayable.** On resume the runner appended a second
  request, leaving more requests than responses forever. The request payload is
  derived only from the task, provider, model, transcript and exposed operations,
  so a resumed attempt regenerates it byte for byte; the runner now reuses the
  abandoned request, and fails loudly if a trailing request differs.
- **World seeds and cent amounts could overflow on PostgreSQL**, whose `INTEGER`
  is 32-bit. Those columns are `BIGINT` there.
- **Event sequence allocation was not portable**: PostgreSQL rejects a bare
  column beside an aggregate. The query groups explicitly, which is identical in
  behaviour on SQLite.
- **Two upserts incremented an unqualified column**, which PostgreSQL rejects as
  ambiguous inside `DO UPDATE SET`.
- **Fork lineage flags were written as Go booleans into integer columns**, which
  only SQLite accepts.

### Changed

- **Line endings are pinned to LF** via `.gitattributes`, so `gofmt -l` and CI
  agree with a Windows checkout.
- Read-only commands (`inspect`, `trace`, `replay`, `evaluate`, `checkpoints`,
  `compare`, `counterfactual`) accept a PostgreSQL DSN and open it with
  `default_transaction_read_only=on`, so the server refuses writes.

### Removed

- **The standalone lease package and `twinwright lease`.** ADR 0018 put leases in
  a database separate from the runs they owned, which makes fencing
  unenforceable: with no transaction spanning both, a stale worker can pass the
  check and commit anyway. Run ownership now lives in the world store. Shipping
  two lease implementations with different guarantees would be a trap for the
  next reader, so the weaker one is gone rather than deprecated in place.
  Ownership is inspected with `twinwright queue --run <run-id>`.
  (ADR 0020 supersedes ADR 0018)

## Earlier work

Milestones 1 through 14 are recorded in `docs/adr/0001` through `0018`: the world
compiler, the multi-service company world, generalized world definitions,
checkpoints and execution forks, the chaos engine, principal authorization,
declarative assertions, the counterfactual debugger, ledger-derived traces,
multiple agent providers, Twinwright Bench, experimental shadow mode, and
optional container execution.
