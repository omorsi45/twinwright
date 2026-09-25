# ADR 0013: Multiple agent providers

Status: accepted, 2026-09-25

The runner already depends only on `agent.Provider.Next`. Milestone 1 shipped OpenAI Responses and scripted fixtures. The roadmap asks for additional providers without coupling world logic, dispatch, or evaluation to a vendor.

One adapter with an internal vendor switch was rejected. Request and response shapes differ, and a shared switch would couple them. Separate adapters behind the existing interface keep each wire format local. OpenAI stays on the Responses API. `ChatCompletionsProvider` posts to `{base}/chat/completions` for local OpenAI-compatible servers. `AnthropicProvider` posts to the Messages API. Both map tool calls into `agent.Message`, store vendor-specific raw content for resume, and record usage when the response includes it. Error bodies pass through the same redactor as OpenAI, including the configured key.

`agent.KnownProvider` is the allowlist used by forks and the CLI: `scripted`, `openai`, `openai-compatible`, and `anthropic`. The chat base URL comes from `--base-url` or `OPENAI_BASE_URL` and is not persisted. Resume, fork, and counterfactual of an `openai-compatible` run must supply it again. Anthropic has no invented default model; `--model` or `ANTHROPIC_MODEL` is required. The API key for chat completions is optional and sent only when set. Consecutive tool results are coalesced into one Anthropic user message, failed tool statuses set `is_error`, and `disable_parallel_tool_use` matches the OpenAI single-tool preference.

Provider and model remain columns on the run and attributes on the trace, so a later benchmark can group by them. This milestone does not add a benchmark report. No live OpenAI, Anthropic, or local-server call has been verified here. Tests use `httptest`. Scripted fixtures remain the default verified path.
