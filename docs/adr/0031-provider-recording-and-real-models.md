# 0031. Provider recording and a real model run

Date: 2026-10-28

Status: accepted; extends ADR 0027

## Context

Replay from the ledger shows that a run's own bookkeeping reproduces. It does
that by answering the runner from the turns the ledger stored, so the adapter that
produced those turns is never in the loop: response parsing, the recorded
metadata and the classification of a failed attempt are all replaced by a stored
result. Every provider test in the repository had the same limit from the other
side. Each answered an adapter from an `httptest.Server` shaped after a vendor's
documentation, which exercises this code and proves nothing about a vendor's
current wire format.

The provider layer also had four gaps that a recording would have carried
forward:

- A turn did not say which response answered it, so two runs could not be
  compared for the model the server actually used or the reason it stopped.
- The adapters read a body through a silent 4 MiB limit, so a longer body reached
  the decoder cut in half.
- A successful response was not masked. A model that repeated the configured key
  in its content or in a tool argument had it written to the ledger. A failed
  request printed the full URL through the standard library's error, userinfo and
  query included.
- A 429, a 5xx or a dropped connection failed the run on the first attempt, while
  a timeout or a truncated completion already got a second.

This ADR records the changes made for those gaps, the cassette format, the script
and CI job that run a real model, and what such a run does and does not prove.

## Decision

### The turn carries the provider's own account

Each adapter stores four fields on the turn it returns:

| Field | OpenAI Responses | OpenAI-compatible chat | Anthropic Messages |
| --- | --- | --- | --- |
| `response_id` | `id` | `id` | `id` |
| `server_model` | `model` | `model` | `model` |
| `finish_reason` | `status` | `choices[0].finish_reason` | `stop_reason` |
| `http_status` | the response status | the response status | the response status |

All four are omitted when empty. A scripted run's ledger is therefore byte for
byte what it was, and `examples/bench/baselines/standard-scripted.json` was not
regenerated.

### Output is bounded and the key is masked on the success path

- `--max-output-tokens` is accepted by every command that can reach a provider.
  It is sent as `max_output_tokens` (Responses) or `max_tokens` (chat) only when
  positive. Anthropic keeps 4096 when it is 0, because its API requires one. A
  negative value is refused.
- A response body is read up to 4 MiB plus one byte. A body at the limit is
  accepted and a longer one is `ErrResponseTooLarge`, which is an ordinary error
  and not an interruption, so it is not retried.
- The configured key is masked with `redact.Secrets` in every output item before
  the item is interpreted, so the content, the tool arguments and the raw items
  kept for replay all derive from the masked text. Error bodies keep the pattern
  masks, since their text is not trusted. A transport error goes through
  `redact.URL` and its cause text is masked, so a base URL with userinfo or a key
  in its query is not printed.

### Retries cover rate limits, server errors and dropped connections

A call is tried at most twice. These outcomes are interruptions and are retried
once:

| Kind | When | Wait before the second attempt |
| --- | --- | --- |
| `timeout` | the attempt's own deadline passed | none |
| `truncated` | the provider said the completion was cut short | none |
| `rate_limited` | HTTP 429 | `Retry-After`, or 1 s |
| `server_error` | HTTP 500, 502, 503 or 504 | `Retry-After`, or 1 s |
| `transport` | the request failed while neither the caller nor the attempt deadline had given up | 1 s |

`Retry-After` is whole seconds or an HTTP date, capped at `MaxRetryAfter` (30 s).
A missing, zero, negative, fractional, unparseable or past value counts as not
sent. The wait goes through `Runner.Sleep`, which returns the context's error if
the run is cancelled during it, and then no second attempt is made. Every other
status, 501, 505 and 529 included, stays an ordinary error. A body that fails
part way through is also an ordinary error; only a failure of the request itself
is a `transport` interruption.

Both attempts are recorded on the turn, as ADR 0027 describes. The
`provider.interrupted` payload gains `status` and `retry_after_ms` (the capped
value) beside `kind`, `attempt`, `reason` and `deadline_ms`, each omitted when it
has no value. Replay reads the interruptions off the recorded turn and never
waits.

A retried attempt is a second provider call and is billed as one. Model calls
remain at-least-once, as ADR 0029 states.

### Cassettes

`twinwright run <scenario> --record-cassette PATH` wraps the adapter's HTTP
transport and appends one JSON line per exchange to PATH, replacing the file if it
exists. `twinwright replay <run> --cassette PATH` re-executes the run through the
run's own adapter, answered from the file, and compares the result with the
recorded run exactly as ledger replay does (`replay.VerifyWith`).

**Format (version 1).** The first line is a header:

```json
{"twinwright_cassette":1,"provider":"openai-compatible","model":"...","max_output_tokens":0}
```

`max_output_tokens` is omitted when no ceiling was configured. It is in the header
because it is part of the request body: a replay has to rebuild the adapter with
the same value or every request hashes differently. Each later line is one
exchange:

| Field | Meaning |
| --- | --- |
| `seq` | 1-based, counting failed round trips |
| `method`, `url` | the request line; the URL has lost its userinfo and query |
| `request_sha256` | SHA-256 of the request body bytes as sent, before masking |
| `request` | the request body with the configured secrets masked; the body itself when it is JSON, a JSON string holding it otherwise |
| `status` | the response status, or 0 when the round trip failed |
| `retry_after` | the `Retry-After` header text, when sent |
| `content_type` | the `Content-Type` header, when sent |
| `error` | set when the round trip did not complete; with `status` 0 there was no response at all |
| `response` | the response body with the configured secrets masked |

The exchange count of a file is its line count minus one. The file is created with
mode 0600 and each exchange is written with one `Write` as it happens. A write that
fails fails that round trip and every later one, because a cassette missing an
exchange would replay as a divergence nobody can explain. The recorder reads at
most one byte past the adapters' 4 MiB limit, so a hostile server cannot fill the
disk and the adapters' too-large refusal reproduces.

**Replay.** The adapter is rebuilt from the run record (provider and model), the
cassette header (the ceiling) and the first exchange (the address; for
`openai-compatible` the trailing `/chat/completions` is removed to give the base
URL). The key is a fixed placeholder, so replay needs neither `--base-url` nor an
API key. The cassette is the adapter's only transport. It has no base transport
and never dials; `TestCassetteNeverDials` replaces the default transport with a
tripwire to show it.

An incoming request is matched to the next exchange by method, then URL path, then
`request_sha256`, and the first field that differs is named in the error with the
`seq`: `cassette divergence: seq 2: request_sha256 differs`. The host and the query
are not compared, so a cassette recorded against one address replays against
another. The cursor does not move on a divergence. Only `Content-Type` and
`Retry-After` are served back. A recorded failure replays as an error and never as
a response. Retry waits are skipped, so a recorded 429 replays at once. A reader
refuses a version it does not know, a header without a provider or model, a `seq`
out of order, and a `status` that is neither a response nor a recorded failure.

**Secrets.** Request headers are never recorded, so an `Authorization` or
`x-api-key` value cannot reach the file. The hash is taken before masking, which is
how a replay with the placeholder key still matches. The masks know the configured
key, and `redact.Secrets` ignores a secret shorter than 8 characters.

`--record-cassette` exists on `run` only: a cassette holds the exchanges of one
run, `bench`, `worker` and `fork` run many runs or part of one, and `resume`
continues a run whose earlier turns were made elsewhere. It is refused for the
scripted provider, which makes no HTTP calls, and with `--enqueue`, since a worker
executes that run and does not record.

### A real model run

`scripts/live-model.sh [BASE_URL] [MODEL]` takes the base URL from its first
argument (default Ollama's `http://127.0.0.1:11434/v1`) and the model from its
second or `$TW_LIVE_MODEL`, and stops with a usage message when there is none. It
builds the CLI and the billing manifest, runs `duplicate-charge` with
`--agent openai-compatible --record-cassette --steps 12`, prints the evaluation
verdict, replays the run from its ledger and then from the cassette, and writes
`live-model-report.json`. Everything it writes goes under `$TW_LIVE_OUT` (a fresh
`mktemp -d` by default). It passes `--provider-timeout` from
`$TW_LIVE_PROVIDER_TIMEOUT`, default 300 s, because CPU inference, including the
first call's model load, can pass the CLI's 90 s default.

The report holds the model, the server's host and port (never the full URL), whether
the evaluation passed, model turns, tool calls, both replay verdicts, the cassette's
exchange count and wall time, plus the run ID and status, the model's digest from
Ollama's `/api/tags` (empty when the endpoint is unavailable), the machine, the Go
version, the git revision, the start time and the per-step times. No key or header
value is written, and the script fails if the value of `OPENAI_API_KEY` appears in
any file it wrote.

It exits non-zero when recording fails, when the run does not reach `completed`,
when ledger replay is not verified, or when cassette replay is not verified. The
model's evaluation verdict is printed and recorded and never gates: a small model
failing a scenario is a result.

The `real-model` job in `.github/workflows/integration.yml` starts the pinned
image `ollama/ollama:0.40.11` with `docker run -d`, pulls `qwen2.5:1.5b`, runs the
script, stops the container, checks the port no longer answers, and replays the
run from the saved database and cassette with the server gone. That replay must
print `"verified":true`. The job uploads the report and the cassette as artifacts.
It sets no provider key. The model is about 1 GB. It was chosen because in a
probe of three attempts through Ollama's `/v1` API it returned a tool call twice,
where `qwen2.5:0.5b` answered in text; that probe says the model can call a tool,
not how often.

The local run's result, with the model, digest, machine and timings, is recorded in
`docs/live-model-run.md`, which is written after the run. No figure from it is
quoted here.

### Live provider tests

`TestLiveOpenAIToolCall` (`TWINWRIGHT_TEST_LIVE_OPENAI=1`, `OPENAI_API_KEY`,
`TW_LIVE_OPENAI_MODEL`) and `TestLiveAnthropicToolCall`
(`TWINWRIGHT_TEST_LIVE_ANTHROPIC=1`, `ANTHROPIC_API_KEY`,
`TW_LIVE_ANTHROPIC_MODEL`) each make one real call offering `getCustomer`. They
require a model turn with a response ID, an HTTP status of 200 and a
`getCustomer` call with a call ID and an `id` argument, and no copy of the key in
the turn. Each skips with a message naming whatever is not set. Nothing runs them
in CI, which keeps hosted-provider keys out of every workflow. A hermetic test
checks the same assertions against turns built by the real adapters and by hand,
so the assertions are not themselves unexercised.

### What a real run does and does not prove

It proves that the chat completions adapter parses what a real server sent, that a
run against that server can be recorded, and that the recording reconstructs the
run offline through the real adapter, with the server stopped and with neither a
key nor an address supplied. The CI job holds exactly that.

It does not prove that the model is any good. One run of a 1.5 billion parameter
model is one sample, and as ADR 0030 says of live reports, one trial cannot tell a
regression from variance. It does not prove that a hosted provider's wire format is
what the OpenAI Responses and Anthropic adapters assume: only the opt-in live
tests exercise those against a vendor, and none had been run when this was
written. It says nothing about another model, server version, machine or day.

The job is not deterministic. A model that produces output the adapter refuses,
such as tool arguments that are not JSON, ends the run `failed` and fails the job.
That is a true report of what happened and not a defect in the replay.

### Replay of a run that did not complete is cut

`replay` requires a `completed` run, and both ledger and cassette replay refuse any
other status. A run that stops at `--steps` is `paused`, and one that hits a
provider error is `failed`. The script records such a run and reports it with
`run_status`, both replay verdicts `false` and a non-zero exit, and does not try to
reconstruct it. Defining what "verified" means for a run with no final state, or
for one that ends on an `error` event whose cause is the provider, is not done
here.

## Consequences and limits

- A recorded per-attempt timeout replays from a cassette as a `transport`
  interruption, and the replay reports a divergence. Deadlines are not simulated.
- A `Retry-After` given as an HTTP date is clock relative, so a replay at another
  time can compute a different wait. It is not verified to replay identically.
- A response body that is not valid UTF-8 is stored as a JSON string.
- Cassettes for forked or resumed runs are not tested, and neither is an Anthropic
  record and replay. The OpenAI Responses and chat completions adapters were
  recorded and replayed in tests.
- Trace export copies an interruption's kind, attempt, reason and deadline but not
  its `status` or `retry_after_ms`. They are in the ledger and in replay.
- `--record-cassette` exists on `run` only.
- A `Do` error that is really a configuration mistake, such as an unsupported URL
  scheme, is a `transport` interruption, so the run waits a second and tries once
  more before it fails.
- A close error on the cassette file is ignored when `run` ends; every write was
  checked when it happened. A full disk was not exercised.
- The success-path mask knows only the configured key. A credential a model makes
  up, or a configured secret under 8 characters, is not recognised.
- A cassette and the script's run output hold a model's prompts and answers, even
  masked. They are written 0600 or into a temporary directory, and
  `live-model-report.json` and `*.cassette.jsonl` are ignored by git so they are
  not committed by accident.
- ADR 0027 said no live provider call had been verified. That stands for the
  hosted vendors. The chat completions adapter is exercised against a real local
  server by the script and the CI job.
