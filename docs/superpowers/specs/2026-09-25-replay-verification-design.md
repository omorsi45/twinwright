# Twinwright replay verification design

Date: 2026-09-25

## Intent and scope

Verify a completed duplicate-charge run by executing its recorded assistant responses against a fresh, isolated copy of the seeded fictional billing world. The verifier never invokes the original model and never writes to the source database. It detects divergence in meaningful execution events, saved tool results, transcript, and final billing state. A successful result means this build can reproduce that completed run under its original compiled manifest.

This is a verification replay for the existing billing vertical slice. It does not fork a run, change an agent decision, reissue external effects, or claim general compatibility across future runtime versions.

## Inputs and contract

The verifier accepts an open source Store, run ID, and validated compiled Manifest. It loads the run, world seed and digest, and ordered ledger. It requires status completed, a matching manifest digest, contiguous event sequences and IDs, matching start metadata, a terminal completion event, known event types, and a usable assistant message for every model response. It rejects runs with failed model turns or dispatch failures, even if later resumed, until recovery replay has an explicit contract. Injected 503 tool errors are supported. Pause events are metadata and do not alter the replayed decisions.

A missing or malformed ledger record, unsupported response, or mismatch produces a failed report or a clear input error. A source database without the expected run remains untouched.

## Reconstruction

Open an in-memory SQLite Store and seed from the recorded seed and manifest digest. Create one replay run with the original run ID, task, provider, model, scenario, and one-shot fault, but the new world ID. The original run ID preserves deterministic refund IDs derived from run ID and call ID. Feed recorded assistant messages through the existing Runner and Dispatcher with a provider that only returns those messages in order. There is no network call. Allow enough steps to complete, and ensure every recorded response is consumed.

## Comparison

Compare ordered semantic events: model.request, model.response, tool.request, tool.response, state.mutation, error, and retry. Decode JSON before comparison so formatting differences are irrelevant. Ignore execution.started world ID, pause/completion metadata, real recorded timestamps, virtual timestamps, and event IDs because the isolated world can have a different instance ID and pause history.

Compare all saved tool results by call ID, operation, arguments, status, and body. Compare the final transcript and every customer, invoice, charge, and refund field in sorted order. This catches both ledger edits and database drift. The verifier returns counts for model turns, tool calls, and compared events on success. On divergence it names the first difference; the CLI prints a JSON report and exits nonzero.

## Boundaries and risks

The package lives at internal/replay and uses the existing provider, runner, dispatcher, compiler manifest, and store interfaces. Store gains a narrow method to create an isolated replay run with a fixed ID. The CLI adds replay <run-id> --manifest PATH --db PATH. No source migration is required.

Recorded model output is an input to verification, not an independent proof that the model would answer the same way today. The current seeded scenario and handlers are deterministic; changing their semantics should cause a replay difference. Future runtime versioning and historical executable availability need a separate compatibility design. This milestone only supports completed runs whose ledger captures a valid model response for every turn.
