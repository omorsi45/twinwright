package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"twinwright/internal/authz"
	"twinwright/internal/compiler"
	"twinwright/internal/crashpoint"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

type ToolCall struct {
	ID          string         `json:"id"`
	OperationID string         `json:"operation_id"`
	Arguments   map[string]any `json:"arguments"`
}
type Message struct {
	Role        string            `json:"role"`
	Content     string            `json:"content,omitempty"`
	ToolCalls   []ToolCall        `json:"tool_calls,omitempty"`
	CallID      string            `json:"call_id,omitempty"`
	OperationID string            `json:"operation_id,omitempty"`
	Status      int               `json:"status,omitempty"`
	RawOutput   []json.RawMessage `json:"raw_output,omitempty"`
	RawBody     string            `json:"raw_body,omitempty"`
	Usage       *Usage            `json:"usage,omitempty"`
	// The provider's own account of the response, recorded so a live run can be
	// compared with a later one. Each is omitted when empty, which is why a
	// scripted run's ledger is byte-identical to what it was before they existed.
	ResponseID   string `json:"response_id,omitempty"`
	ServerModel  string `json:"server_model,omitempty"`
	FinishReason string `json:"finish_reason,omitempty"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	// Interruptions are the attempts that produced no turn before this one did.
	// They live on the message because the message is what the ledger records and
	// what a replay reads back: the recorded turn is what makes the interruption
	// reproducible without the provider having to fail again.
	Interruptions []Interruption `json:"interruptions,omitempty"`
}

// Usage is the token count a provider reported for one turn.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}
type Provider interface {
	Next(context.Context, string, []Message, []compiler.Operation) (Message, error)
}
type Runner struct {
	Store    *store.Store
	Dispatch *dispatch.Dispatcher
	Manifest compiler.Manifest
	Provider Provider
	// Fence, when set, makes every durable write conditional on this worker
	// still owning the run. A nil Fence is the single-node local path and
	// behaves exactly as before. Clock is for deterministic lease-expiry
	// tests; nil means time.Now.
	Fence *store.Fence
	Clock store.Clock
	// Crash, when set, is offered the model-call and completion boundaries so
	// a harness can simulate process death at each. When it fires, Execute
	// returns the crash at once and writes nothing more, as a dead process
	// would.
	Crash crashpoint.Hook
	// Sleep pauses between two attempts at one model turn after a rate limit, a
	// server error or a transport failure. It must return the context's error if
	// the context ends first. Nil means a context-aware timer; a test supplies
	// one so a retry costs no time. Replay from the ledger never reaches it: a
	// replayed turn carries its interruptions already and arrives without an
	// error. Replay through a real adapter (replay.VerifyWith) does reach it, and
	// supplies one that returns at once.
	Sleep func(ctx context.Context, d time.Duration) error
}

// sleep waits through the configured Sleep, or a timer when none is set.
func (r Runner) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	return sleepContext(ctx, d)
}

func (r Runner) Execute(ctx context.Context, runID string, maxSteps int) (store.Run, error) {
	if maxSteps < 1 {
		return store.Run{}, fmt.Errorf("maxSteps must be positive")
	}
	if r.Provider == nil {
		return store.Run{}, fmt.Errorf("provider is required")
	}
	used := 0
	for {
		run, err := r.Store.Run(ctx, runID)
		if err != nil {
			return store.Run{}, err
		}
		if run.Status == "completed" {
			return run, nil
		}
		var history []Message
		if err = json.Unmarshal([]byte(run.Transcript), &history); err != nil {
			return store.Run{}, err
		}
		if call, ok := pending(history); ok {
			if _, err = r.Dispatch.Invoke(ctx, runID, call.ID, call.OperationID, call.Arguments); err != nil {
				if errors.Is(err, crashpoint.ErrCrashed) {
					// A simulated death records nothing: no error event, no
					// failed status.
					return store.Run{}, err
				}
				payload := map[string]any{"kind": "dispatch", "call_id": call.ID, "operation_id": call.OperationID, "message": err.Error()}
				if saveErr := r.Store.SaveStatusGuarded(ctx, r.Fence, r.Clock, runID, "failed", "error", payload); saveErr != nil {
					return store.Run{}, fmt.Errorf("dispatch error: %v; recording error: %w", err, saveErr)
				}
				return store.Run{}, err
			}
			continue
		}
		if len(history) > 0 && history[len(history)-1].Role == "assistant" && len(history[len(history)-1].ToolCalls) == 0 {
			if err = crashpoint.Fire(ctx, r.Crash, crashpoint.Site{Point: crashpoint.BeforeCompletion, RunID: runID, Step: run.Step}); err != nil {
				return store.Run{}, err
			}
			if err = r.Store.SaveStatusGuarded(ctx, r.Fence, r.Clock, runID, "completed", "execution.completed", map[string]any{"step": run.Step}); err != nil {
				return store.Run{}, err
			}
			return r.Store.Run(ctx, runID)
		}
		if used >= maxSteps {
			if err = r.Store.SaveStatusGuarded(ctx, r.Fence, r.Clock, runID, "paused", "execution.paused", map[string]any{"step": run.Step}); err != nil {
				return store.Run{}, err
			}
			return r.Store.Run(ctx, runID)
		}
		operations, err := authz.Exposed(ctx, r.Store.DB, runID, r.Manifest.Operations)
		if err != nil {
			return store.Run{}, err
		}
		request := map[string]any{"task": run.Task, "provider": run.Provider, "model": run.Model, "history": history, "operations": operations}
		site := crashpoint.Site{RunID: runID, Step: run.Step}
		site.Point = crashpoint.BeforeModelRequest
		if err = crashpoint.Fire(ctx, r.Crash, site); err != nil {
			return store.Run{}, err
		}
		if err = r.startModelCall(ctx, runID, request); err != nil {
			return store.Run{}, err
		}
		site.Point = crashpoint.AfterModelRequest
		if err = crashpoint.Fire(ctx, r.Crash, site); err != nil {
			return store.Run{}, err
		}
		next, err := r.callProvider(ctx, run.Task, history, operations)
		if err != nil {
			if saveErr := r.Store.FailModelTurnGuarded(ctx, r.Fence, r.Clock, runID, next, interruptionPayloads(next.Interruptions), err.Error()); saveErr != nil {
				return store.Run{}, fmt.Errorf("provider error: %v; recording error: %w", err, saveErr)
			}
			return store.Run{}, err
		}
		if next.Role != "assistant" {
			err = fmt.Errorf("provider returned role %q", next.Role)
		}
		if err == nil {
			err = r.validateCalls(history, next)
		}
		if err != nil {
			if saveErr := r.Store.FailModelTurnGuarded(ctx, r.Fence, r.Clock, runID, next, interruptionPayloads(next.Interruptions), err.Error()); saveErr != nil {
				return store.Run{}, fmt.Errorf("validation error: %v; recording error: %w", err, saveErr)
			}
			return store.Run{}, err
		}
		history = append(history, next)
		transcript, err := json.Marshal(history)
		if err != nil {
			return store.Run{}, err
		}
		if err = r.Store.SaveTurnGuarded(ctx, r.Fence, r.Clock, runID, run.Step+1, string(transcript), next, interruptionPayloads(next.Interruptions)); err != nil {
			return store.Run{}, err
		}
		site.Point = crashpoint.AfterModelResponse
		if err = crashpoint.Fire(ctx, r.Crash, site); err != nil {
			return store.Run{}, err
		}
		used++
	}
}

// validateCalls checks the tool calls of a turn the provider just returned.
//
// Every call needs an ID and a compiled operation. An ID may also be used only
// once in a run. pending() treats a call as done once any tool message carries
// its ID, so a reused ID would be skipped without ever being dispatched and the
// model would never receive a result for that call. Failing the turn is the
// only outcome that does not hide that from the model and the ledger.
func (r Runner) validateCalls(history []Message, next Message) error {
	seen := map[string]bool{}
	for _, message := range history {
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				seen[call.ID] = true
			}
		}
	}
	inTurn := map[string]bool{}
	for _, call := range next.ToolCalls {
		if call.ID == "" || r.Manifest.Operation(call.OperationID) == nil {
			return fmt.Errorf("provider returned invalid tool call")
		}
		if inTurn[call.ID] {
			return fmt.Errorf("provider returned invalid tool call: call ID %q appears twice in one turn", call.ID)
		}
		if seen[call.ID] {
			return fmt.Errorf("provider reused call ID %q from an earlier turn", call.ID)
		}
		inTurn[call.ID] = true
	}
	return nil
}

// providerAttempts is how many times one model turn is attempted.
//
// Two, not more. An attempt that passed its deadline, came back cut short, was
// rate limited, hit a server error or lost its connection is routinely transient,
// and killing a long run over the first one is the wrong trade. The second
// failure is a different signal: whatever is wrong is not something waiting again
// will fix, and a third attempt only delays the report while spending another
// call. Nothing escalates silently either way, because every attempt is recorded.
const providerAttempts = 2

// callProvider makes up to providerAttempts attempts at one model turn and
// returns the turn together with the attempts that produced nothing.
//
// Only an interrupted attempt is retried. An ordinary provider error - a rejected
// key, a malformed request, a 400 - is returned as it always was, because a retry
// cannot fix it and two identical failures are worse evidence than one.
//
// A rate limit, a server error or a transport failure is retried after a wait:
// the server's Retry-After up to MaxRetryAfter, or one second when it named none.
// A timeout or a truncation is retried at once. The wait goes through r.Sleep and
// ends early, with the context's error, if the run is cancelled during it.
//
// A replayed turn arrives with its interruptions already on it and no error, so
// the recorded list is what gets appended in both cases. That is what lets a run
// that was interrupted once replay from its own ledger without the provider
// having to be made to fail again, and without waiting out a recorded Retry-After.
func (r Runner) callProvider(ctx context.Context, task string, history []Message, ops []compiler.Operation) (Message, error) {
	var recorded []Interruption
	for attempt := 1; ; attempt++ {
		next, err := r.Provider.Next(ctx, task, history, ops)
		var interrupt *CallInterrupted
		if !errors.As(err, &interrupt) {
			next.Interruptions = append(recorded, next.Interruptions...)
			return next, err
		}
		recorded = append(recorded, interrupt.Record(attempt))
		if attempt >= providerAttempts {
			next.Interruptions = recorded
			return next, fmt.Errorf("provider interrupted on %d of %d attempts: %w", len(recorded), providerAttempts, err)
		}
		if wait := interrupt.wait(); wait > 0 {
			if sleepErr := r.sleep(ctx, wait); sleepErr != nil {
				next.Interruptions = recorded
				return next, sleepErr
			}
		}
	}
}

// interruptionPayloads hands the store what to record without giving it a reason
// to know about this package's types.
func interruptionPayloads(interruptions []Interruption) []any {
	if len(interruptions) == 0 {
		return nil
	}
	out := make([]any, 0, len(interruptions))
	for _, interruption := range interruptions {
		out = append(out, interruption)
	}
	return out
}

// startModelCall records the model request the runner is about to make,
// idempotently.
//
// A process that dies between persisting a model.request and recording the
// model.response leaves the ledger ending in an unanswered request. Appending a
// second request on resume would leave the ledger with more requests than
// responses forever, and replay - which pairs them - would reject the run for
// the rest of its life. Because the request payload is derived only from the
// task, provider, model, transcript and exposed operations, a resumed attempt
// regenerates it byte for byte, so the abandoned request IS this attempt's
// request and is reused.
//
// A trailing request whose payload differs is not a resumed attempt: something
// about the run changed underneath us (a different manifest or policy), and
// continuing would silently attribute one request to a different context. That
// fails loudly instead.
func (r Runner) startModelCall(ctx context.Context, runID string, request map[string]any) error {
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	last, err := r.Store.LastEvent(ctx, runID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case last.Type == "model.request":
		if equalJSON(last.Payload, encoded) {
			return nil
		}
		return fmt.Errorf("run %s has an abandoned model request that differs from the reconstructed one; the run cannot be resumed safely", runID)
	}
	return r.Store.AppendGuarded(ctx, r.Fence, r.Clock, runID, "model.request", request)
}

// equalJSON compares two encodings by value, so key order cannot make an
// identical request look different.
func equalJSON(a, b []byte) bool {
	var left, right any
	if err := json.Unmarshal(a, &left); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &right); err != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

func pending(history []Message) (ToolCall, bool) {
	done := map[string]bool{}
	for _, message := range history {
		if message.Role == "tool" {
			done[message.CallID] = true
		}
	}
	for _, message := range history {
		if message.Role == "assistant" {
			for _, call := range message.ToolCalls {
				if !done[call.ID] {
					return call, true
				}
			}
		}
	}
	return ToolCall{}, false
}
