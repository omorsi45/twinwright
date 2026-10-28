# Agent providers, recording and real models

This page was moved out of the project README when the README was rewritten as the Twinwright Cloud overview. It holds the README section "Agent providers", unchanged except where noted. The statement that no live run had been verified was corrected, and the protocol link was adjusted. The sections "The output ceiling", "Rate limits, server errors and dropped connections", "Recording a run, and replaying it through the adapter", "A real model, once" and "Gating a run" at the end are new. Paths in backticks are relative to the repository root. [Back to the guide index](README.md).

## Agent providers

`--agent` selects the provider. The world, dispatcher, and evaluator do not learn vendor details. Each adapter maps a vendor response into Twinwright's internal message shape and keeps the raw response for the ledger.

| Agent | Endpoint | Credentials / config |
| --- | --- | --- |
| `scripted` | in-process fixtures | none |
| `openai` | OpenAI Responses API | `OPENAI_API_KEY`; model defaults to `gpt-6-sol`, or `OPENAI_MODEL` / `--model` |
| `openai-compatible` | `{base}/chat/completions` | `--base-url` or `OPENAI_BASE_URL`; optional `OPENAI_API_KEY`; model required via `--model` or `OPENAI_MODEL` |
| `anthropic` | Anthropic Messages API | `ANTHROPIC_API_KEY`; model required via `--model` or `ANTHROPIC_MODEL` (no invented default) |
| `exec` | your own program, JSON over stdin and stdout | `--model` naming the agent and its version, and `--agent-command`; see [the protocol](../protocol/agent-v1.md) |

```bash
export OPENAI_API_KEY="..."            # or ANTHROPIC_API_KEY, or OPENAI_BASE_URL
go run ./cmd/twinwright run company-incident \
  --agent openai \
  --manifest company.world.manifest.json \
  --db company.db
```

Swap `--agent` for `anthropic` or `openai-compatible` and supply that row's
credentials; `openai-compatible` also needs `--model` and a base URL. The base URL
is process configuration, not a ledger column, so resuming such a run needs
`--base-url` or `OPENAI_BASE_URL` again. The stored provider name and model are what
replay and traces show.

`--agent exec` runs an agent of your own as a separate process, started afresh for
every turn. It proposes tool calls; the dispatcher still authorizes and executes
them. The command line is not stored with the run, so resuming an exec run or
serving it from a worker needs `--agent-command` again. The protocol, its limits
and a Python reference agent are in `docs/protocol/agent-v1.md` and
`examples/agents/python/`. `--agent-container IMAGE` runs the agent in a hardened
docker container with no network by default; what that does and does not protect
against is in `docs/adr/0033-agent-protocol-and-isolation.md`.

Twinwright's world state, tool execution, recorded decisions, and deterministic evaluators provide the reproducible boundary around it. When a provider reports token usage, the assistant turn stores it. Error bodies are redacted before they reach the ledger. A run against a local Ollama model is recorded in `docs/live-model-run.md`, and the Integration workflow's `real-model` job runs one (see "A real model, once" below). Hosted providers are unverified: `TestLiveOpenAIToolCall` and `TestLiveAnthropicToolCall` make one real call each, are opt-in, and have never run in this repository's CI. Scripted fixtures remain the default tested path. See `docs/adr/0013-agent-providers.md`.

### When a provider hangs or cuts a completion short

`--provider-timeout` is the per-attempt deadline, default 90s, available on every
command that can reach a provider. It arrives on the request's context, so a hung
call is cancelled rather than abandoned, and a caller's own deadline still wins. It
cannot be zero: an uncancellable call is what this replaced.

A completion the provider cut off at its token ceiling arrives as HTTP 200 with a
well-formed body, and the only signal is a metadata field spelled differently on
each surface: `stop_reason: "max_tokens"` on Anthropic's Messages API,
`finish_reason: "length"` on an OpenAI-compatible chat completion, and
`status: "incomplete"` with `incomplete_details.reason: "max_output_tokens"` on the
Responses API. Twinwright reads it and returns before parsing any output item, so a
tool call cut mid-arguments never becomes a dispatched action.
`--model`-specific ceilings are the operator's to raise; `AnthropicProvider`
takes a configurable `MaxOutputTokens` because the API requires one.

Either failure is retried exactly once and both attempts are recorded as
`provider.interrupted` events carrying the kind, the attempt number, the vendor's
verbatim reason and the configured deadline:

```json
{"kind":"timeout","attempt":1,"deadline_ms":90000}
{"kind":"truncated","attempt":1,"reason":"length"}
```

A run that recovers on the second attempt still replays: the interruption is
recorded on the turn itself, so a replayed run reproduces the same ledger from the
record instead of needing the provider to fail again. A run where every attempt is
interrupted fails with the usual `error` event of kind `provider`.

**This detection is only as good as the field.** A surface that omits its stop
reason, or a gateway that rewrites it to a success value, gives nothing to check
and the response is treated as complete. There is no content-level alternative: a
truncated response is a successful request whose content happens to be a fragment.
See `docs/adr/0027-provider-deadlines-and-truncation.md`.

### The output ceiling

`--max-output-tokens N` sets the most tokens a model may produce in one call. Every command that can reach a provider accepts it. `0`, the default, keeps each provider's own behaviour: an OpenAI-compatible server is sent no ceiling, and Anthropic is sent 4096 because its API requires one. A negative value is refused. This is the remedy for the truncation described above: when a completion is cut off at the ceiling, raise it. The cassette header records the ceiling that was in force. `TestMaxOutputTokensReachesTheRequest`, `TestSelectProviderCarriesTheOutputCeiling` and `TestCassetteHeaderCarriesTheOutputCeiling` hold it. See `docs/adr/0031-provider-recording-and-real-models.md`.

### Rate limits, server errors and dropped connections

Three more failures are interruptions, retried once like a timeout or a truncation: a 429 is kind `rate_limited`, a 500, 502, 503 or 504 is `server_error`, and a request that failed before any response arrived is `transport`. The recorded kinds are therefore `timeout`, `truncated`, `rate_limited`, `server_error` and `transport`. Every other non-2xx status, 501 and 505 included, is an ordinary error and is not retried. The one exception is the Anthropic adapter, which also treats 529, Anthropic's documented `overloaded_error`, as `server_error` (`TestAnthropicOverloadedIsRetriedOnce`); the other two adapters keep it an ordinary error. A body that fails part way through is an ordinary error too (`TestBodyFailurePartWayIsNotRetried`).

The `provider.interrupted` event for the three new kinds also carries the HTTP status and the server's `Retry-After` in milliseconds as `status` and `retry_after_ms`. The wait before the second attempt is the server's `Retry-After` (whole seconds or an HTTP date) capped at 30 seconds, or one second when none was sent or it was unusable. A timeout or a truncation retries at once. Cancelling the run during the wait returns the context error with no second attempt. Replay never waits, because a replayed turn carries its interruptions already. A response body over 4 MiB is an error (`ErrResponseTooLarge`) and is not retried. Transport error text goes through the redactor, so a URL's userinfo and query never reach the ledger, and neither does the error from a base URL that does not parse (`TestMalformedBaseURLNeverLeaks`); `run` refuses such a `--base-url` before it creates a run. A base URL's own query is kept and `/chat/completions` is added to its path (`TestChatBaseURLQueryIsKept`). See `TestRateLimitedAttemptIsRetriedAndRecorded`, `TestServerErrorIsRetriedOnce`, `TestTransportFailureIsRetriedAndRecorded`, `TestRetriedRunStillReplays` and `docs/adr/0031-provider-recording-and-real-models.md`.

### Recording a run, and replaying it through the adapter

Replay from the ledger reproduces a run's own bookkeeping but never runs the adapter that produced the turns. A cassette puts the adapter in the loop.

```bash
go run ./cmd/twinwright run duplicate-charge \
  --agent openai-compatible --model qwen2.5:1.5b --base-url http://127.0.0.1:11434/v1 \
  --record-cassette run.cassette.jsonl --db run.db

go run ./cmd/twinwright replay <run-id> --cassette run.cassette.jsonl --db run.db
```

`run --record-cassette PATH` writes every HTTP exchange of the provider to a JSON-lines file: a header line naming the provider, model and output ceiling, then one line per exchange. `replay <run> --cassette PATH` re-executes the run through the run's own adapter, answered from that file, so response parsing, the recorded metadata and the interruption classification are in the loop. Replay needs neither `--base-url` nor an API key, and the cassette transport never dials (`TestCassetteNeverDials`). A request is matched by method, URL path and request hash, and a divergence names the exchange and the first field that differs (`TestCassetteDivergenceNamesTheExchange`). A redirect is recorded as its own exchange with its `Location` (without query), so a run whose endpoint redirected replays (`TestCassetteReplaysARedirectedExchange`). The file is mode 0600, also when it replaces an existing one (`TestRecordingTransportReplacesWithMode0600`). Request headers are never recorded, a URL loses its userinfo and query, and both bodies are masked with the configured key (`TestCassetteHoldsNoSecrets`); the hash is taken over the bytes as sent, so a replay with a placeholder key still matches.

Limits. `--record-cassette` exists on `run` only, and it is refused for the scripted provider, for `exec` and with `--enqueue`. A per-attempt timeout that passes before the response headers arrive replays as a `transport` interruption, and one that passes while the body is read replays as an ordinary error; either way the replay reports a divergence. A `Retry-After` given as an HTTP date is clock relative and is not verified to replay identically. A response body that is not valid UTF-8 is stored as a JSON string. Cassettes for forked or resumed runs, and an Anthropic record and replay, are not tested. **A cassette holds the model's prompts and answers, so do not commit one.** `*.cassette.jsonl` is in `.gitignore`.

### A real model, once

`scripts/live-model.sh [BASE_URL] [MODEL]` (`make live-model`) builds the CLI, runs `duplicate-charge` with `--agent openai-compatible --record-cassette` against a local Ollama server, prints the evaluation verdict, replays the run from its ledger and then from the cassette, and writes `live-model-report.json`. It exits non-zero when recording fails, when the run does not reach `completed`, or when either replay is not verified. The evaluation verdict is reported and never gates, because a small model failing a scenario is a result and not a broken build.

The run recorded in `docs/live-model-run.md` used `qwen2.5:1.5b` on a CPU: it completed after 2 model turns and 1 tool call, did not solve the scenario, and both replays verified, including one with the model server stopped. That is one sample on one machine. It shows the chat completions adapter parsed a real server's output and that the recording reconstructed the run offline; it does not show that the model is competent, that the result repeats, or anything about a hosted provider. The `real-model` job of the Integration workflow runs the same script against the pinned `ollama/ollama:0.40.11` image, stops the container and replays from the cassette alone. See `docs/adr/0031-provider-recording-and-real-models.md`.

### Gating a run

`run --fail-on-eval` exits non-zero after printing the result when the run's evaluation fails. It is opt-in, so existing scripts keep working, and it cannot be combined with `--enqueue`, because an enqueued run is evaluated by a worker and not by the command that queued it (`TestRunFailOnEval`, `TestRunRefusesFailOnEvalWithEnqueue`). To compare a whole bench report with an approved baseline instead of judging one run, use `twinwright gate`: see [the bench page](bench.md) and `docs/adr/0030-regression-gate.md`.
