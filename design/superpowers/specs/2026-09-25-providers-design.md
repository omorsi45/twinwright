# Milestone 10: multiple agent providers design

Roadmap section: `briefs/2026-09-25-platform-roadmap.md`, "MILESTONE 10: MULTIPLE AGENT PROVIDERS" (line 978).

## Intent

Add provider adapters without teaching the world, dispatcher, or evaluator anything about a vendor. Each adapter turns one vendor response into `agent.Message` and keeps the raw response for the ledger. Scripted fixtures stay the tested default. No live call is claimed.

## Approaches

1. One adapter with a vendor switch. Rejected: request and response shapes differ, and a switch would couple them.
2. Separate adapters behind the existing `agent.Provider` interface (chosen). The runner already depends only on `Next`. OpenAI stays on the Responses API. A new chat-completions adapter covers local OpenAI-compatible servers. An Anthropic adapter covers the Messages API.

## Design

- `agent.KnownProvider` is the allowlist: `scripted`, `openai`, `openai-compatible`, `anthropic`. Fork validation uses it.
- `ChatCompletionsProvider` posts to `{base}/chat/completions`. The base URL comes from `--base-url` or `OPENAI_BASE_URL`. The API key is optional and sent only when set. Tool calls use the function-tool shape. Usage maps `prompt_tokens`, `completion_tokens`, and `total_tokens` onto `Message.Usage`. The assistant message is stored as `RawOutput`.
- `AnthropicProvider` posts to the Messages API (`ANTHROPIC_API_KEY`, `--model` or `ANTHROPIC_MODEL`, no invented default model). `max_tokens` is 4096. Tool use blocks become tool calls. Content blocks are stored as `RawOutput`. Usage maps input and output tokens; total is their sum when both are present.
- Both adapters redact error bodies with the same redactor as OpenAI, including their own key.
- The endpoint is process configuration, not a ledger column. Resume of an `openai-compatible` run needs the base URL again. The stored provider name and model are what replay and traces show.
- Provider and model are already attributes on the trace and columns on the run, so a later benchmark can group by them. This milestone does not add a benchmark report.

## Limits

- No live OpenAI, Anthropic, or local-server run has been verified. Tests use `httptest`.
- Chat-completions servers that reject unknown fields are given only the common fields. Parallel tool calls are not disabled in the request; the runner still executes returned calls one at a time.
- Anthropic resume resends stored raw content blocks when they exist, and plain text otherwise.
- The base URL is not persisted.
