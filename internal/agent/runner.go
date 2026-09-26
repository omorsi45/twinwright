package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"twinwright/internal/authz"
	"twinwright/internal/compiler"
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
				payload := map[string]any{"kind": "dispatch", "call_id": call.ID, "operation_id": call.OperationID, "message": err.Error()}
				if saveErr := r.Store.SaveStatusGuarded(ctx, r.Fence, r.Clock, runID, "failed", "error", payload); saveErr != nil {
					return store.Run{}, fmt.Errorf("dispatch error: %v; recording error: %w", err, saveErr)
				}
				return store.Run{}, err
			}
			continue
		}
		if len(history) > 0 && history[len(history)-1].Role == "assistant" && len(history[len(history)-1].ToolCalls) == 0 {
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
		if err = r.startModelCall(ctx, runID, request); err != nil {
			return store.Run{}, err
		}
		next, err := r.Provider.Next(ctx, run.Task, history, operations)
		if err != nil {
			if saveErr := r.Store.FailModelTurnGuarded(ctx, r.Fence, r.Clock, runID, next, err.Error()); saveErr != nil {
				return store.Run{}, fmt.Errorf("provider error: %v; recording error: %w", err, saveErr)
			}
			return store.Run{}, err
		}
		if next.Role != "assistant" {
			err = fmt.Errorf("provider returned role %q", next.Role)
		}
		if err == nil {
			for _, call := range next.ToolCalls {
				if call.ID == "" || r.Manifest.Operation(call.OperationID) == nil {
					err = fmt.Errorf("provider returned invalid tool call")
					break
				}
			}
		}
		if err != nil {
			if saveErr := r.Store.FailModelTurnGuarded(ctx, r.Fence, r.Clock, runID, next, err.Error()); saveErr != nil {
				return store.Run{}, fmt.Errorf("validation error: %v; recording error: %w", err, saveErr)
			}
			return store.Run{}, err
		}
		history = append(history, next)
		transcript, err := json.Marshal(history)
		if err != nil {
			return store.Run{}, err
		}
		if err = r.Store.SaveTurnGuarded(ctx, r.Fence, r.Clock, runID, run.Step+1, string(transcript), next); err != nil {
			return store.Run{}, err
		}
		used++
	}
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
