package agent

import (
	"context"
	"encoding/json"
	"fmt"

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
}
type Provider interface {
	Next(context.Context, string, []Message, []compiler.Operation) (Message, error)
}
type Runner struct {
	Store    *store.Store
	Dispatch *dispatch.Dispatcher
	Manifest compiler.Manifest
	Provider Provider
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
				if saveErr := r.Store.SaveStatus(ctx, runID, "failed", "error", payload); saveErr != nil {
					return store.Run{}, fmt.Errorf("dispatch error: %v; recording error: %w", err, saveErr)
				}
				return store.Run{}, err
			}
			continue
		}
		if len(history) > 0 && history[len(history)-1].Role == "assistant" && len(history[len(history)-1].ToolCalls) == 0 {
			if err = r.Store.SaveStatus(ctx, runID, "completed", "execution.completed", map[string]any{"step": run.Step}); err != nil {
				return store.Run{}, err
			}
			return r.Store.Run(ctx, runID)
		}
		if used >= maxSteps {
			if err = r.Store.SaveStatus(ctx, runID, "paused", "execution.paused", map[string]any{"step": run.Step}); err != nil {
				return store.Run{}, err
			}
			return r.Store.Run(ctx, runID)
		}
		request := map[string]any{"task": run.Task, "provider": run.Provider, "model": run.Model, "history": history, "operations": r.Manifest.Operations}
		if err = r.Store.StartModelCall(ctx, runID, request); err != nil {
			return store.Run{}, err
		}
		next, err := r.Provider.Next(ctx, run.Task, history, r.Manifest.Operations)
		if err != nil {
			if saveErr := r.Store.FailModelTurn(ctx, runID, map[string]any{"error": err.Error()}, err.Error()); saveErr != nil {
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
			if saveErr := r.Store.FailModelTurn(ctx, runID, next, err.Error()); saveErr != nil {
				return store.Run{}, fmt.Errorf("validation error: %v; recording error: %w", err, saveErr)
			}
			return store.Run{}, err
		}
		history = append(history, next)
		transcript, err := json.Marshal(history)
		if err != nil {
			return store.Run{}, err
		}
		if err = r.Store.SaveTurn(ctx, runID, run.Step+1, string(transcript), next); err != nil {
			return store.Run{}, err
		}
		used++
	}
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
