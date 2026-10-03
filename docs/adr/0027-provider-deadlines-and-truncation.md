# 0027. Provider deadlines, truncated completions, and the interrupted attempt

Date: 2026-10-02

Status: accepted

## Context

Every provider did the same thing with its HTTP client:

```go
client := p.Client
if client == nil {
    client = &http.Client{Timeout: 90 * time.Second}
}
```

Flat, hard-coded, not per attempt, and not cancellable by the caller: a hung
provider took 90 seconds off the run and then returned a transport error with
nothing in the ledger saying what had happened.

The second gap was quieter and more dangerous. Nothing in the repository read
`finish_reason`, `stop_reason` or any equivalent, and `anthropic.go` hard-coded
`"max_tokens": 4096`. A completion the provider cut off at that ceiling arrives as
HTTP 200 with a well-formed body; the only thing that says it is a fragment is one
metadata field. A tool call cut mid-arguments can still decode into a map, so the
runtime would dispatch an action built from half a serialisation and record it as
a decision the model made.

The field is spelled differently on each surface, which is why reading it has to
be done per provider rather than once:

| Surface | Field | Truncation value |
| --- | --- | --- |
| Anthropic Messages | `stop_reason` | `max_tokens`, and `model_context_window_exceeded` for the model's own window |
| OpenAI Chat Completions | `choices[].finish_reason` | `length`, plus `model_length` in Mistral's own SDK |
| OpenAI Responses | `status` plus `incomplete_details.reason` | `incomplete` with `max_output_tokens` |

## Decision

**The deadline is per attempt and arrives on the context.** Each provider derives
`context.WithTimeout(ctx, p.Timeout)` for its own attempt, and the client no
longer carries a timeout of its own. The request is cancelled rather than
abandoned, a caller's own deadline still wins, and `--provider-timeout` sets it on
every command that can reach a provider. It cannot be set to zero: an
uncancellable call is the thing this replaced.

**An interrupted attempt is a named outcome, not an error string.**
`agent.CallInterrupted` carries a kind - `timeout` or `truncated` - and the
provider's verbatim reason. Only the attempt's own deadline firing makes a
timeout: a caller whose context was cancelled is the run going away, and retrying
a call nobody is waiting for is worse than reporting the cancellation.

**A truncated completion returns before any output item is parsed.** No tool call,
no content, no usage: just the raw body for diagnosis and the interruption. This
is the half of the change with teeth, because the alternative is a fragment that
parses.

**One retry, and both attempts are recorded.** An attempt that passed its deadline
or came back cut short is retried once. The first such failure is routinely
transient and killing a long run over it is the wrong trade; a second identical
failure is a different signal, and a third attempt only delays the report while
spending another call. If every attempt is interrupted the run fails with the
existing `error` event of kind `provider`, so nothing downstream has to learn a
new error vocabulary.

Retrying a truncated completion deserves its own note, because the vendor's
remedy is a *different* request at a higher ceiling, which the runner cannot
invent on the caller's behalf. What the retry buys is the case where a long
reasoning preamble consumed the budget and a second sample does not; what it
does not do is pretend the ceiling changed. `AnthropicProvider.MaxOutputTokens`
makes the ceiling configurable so an operator can apply the real remedy, and a
run that truncates twice fails with both attempts in the ledger rather than
quietly continuing on a fragment.

**The record lives on the turn, and that is what makes it replayable.**
`provider.interrupted` events are appended in the same transaction as the
`model.response` they precede, from `Message.Interruptions` on the turn itself.
Three things follow:

- A crash between the request and the response leaves the ledger ending in an
  unanswered `model.request`, exactly as before, so the existing resume rule still
  holds. Appending the interruption separately would leave an abandoned
  interruption under that request, and the resumed attempt would record its own:
  the ledger would then hold more interruptions than the turn carries.
- A replayed run reads the interruptions off the recorded turn and appends the
  same events, so the two ledgers match without anyone having to make a provider
  hang again. This is how `chaos` already works: the record carries the fault.
- `provider.interrupted` is therefore in replay's semantic list, which means
  replay compares it field for field rather than ignoring it. The payload carries
  nothing measured from a clock: kind, attempt number, the vendor's reason, and
  the configured deadline in milliseconds.

Checkpoint discovery accepts the event inside an open model call and does not
treat it as a boundary - nothing was decided and nothing was written, so there is
nothing to fork from. Reconstruction replays it from the recorded turn, so a fork
past an interrupted turn rebuilds a prefix that matches event for event.

## Consequences

A run that survives a hung provider call now says so, in a form `twinwright
trace` puts on the model span and an assertion file can match on
(`provider.interrupted` is in the assertion schema's event types).

Two limits are worth stating plainly rather than leaving to be discovered.

**Detection requires the provider to send the field.** A surface that omits its
stop reason, or a gateway that rewrites it to a success value - vLLM shipped
exactly that bug for streamed tool calls - gives nothing to check, and this code
will treat the response as complete. There is no content-level alternative: a
truncated response is a successful request whose content happens to be a
fragment.

**No live provider call has been verified.** There is no API key on this host.
Both behaviours are tested against an `httptest.Server` that hangs and one that
returns each surface's documented truncation shape, which exercises this code but
proves nothing about any vendor's current wire format beyond what their
documentation states.
