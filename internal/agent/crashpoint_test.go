package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"twinwright/internal/compiler"
	"twinwright/internal/crashpoint"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

// crashAt returns a hook that simulates process death at the first site of the
// given point (for tool points, the first site for operation). It records the
// sites it was offered so a test can prove the point was reached.
func crashAt(point crashpoint.Point, operation string, offered *[]crashpoint.Site) crashpoint.Hook {
	return func(_ context.Context, site crashpoint.Site) error {
		*offered = append(*offered, site)
		if site.Point != point {
			return nil
		}
		if operation != "" && site.OperationID != operation {
			return nil
		}
		return errors.New("test crash")
	}
}

func TestCrashPointsLeaveTheDocumentedDurableState(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "billing")
	spec, err := os.ReadFile(filepath.Join(root, "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := os.ReadFile(filepath.Join(root, "bindings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := compiler.Compile(spec, bindings)
	if err != nil {
		t.Fatal(err)
	}
	const refundCall = "fixture-refund"
	tests := []struct {
		point     crashpoint.Point
		operation string
		check     func(t *testing.T, d durableState)
	}{
		{crashpoint.BeforeModelRequest, "", func(t *testing.T, d durableState) {
			if d.requests != d.responses {
				t.Fatalf("requests=%d responses=%d, want equal", d.requests, d.responses)
			}
			if d.requests != 0 {
				t.Fatalf("requests=%d, a first-occurrence crash precedes every request", d.requests)
			}
			if d.lastEvent == "model.request" {
				t.Fatal("last event is a model.request")
			}
		}},
		{crashpoint.AfterModelRequest, "", func(t *testing.T, d durableState) {
			if d.lastEvent != "model.request" {
				t.Fatalf("last event=%q, want model.request", d.lastEvent)
			}
			if d.requests != d.responses+1 {
				t.Fatalf("requests=%d responses=%d, want one unanswered request", d.requests, d.responses)
			}
		}},
		{crashpoint.AfterModelResponse, "", func(t *testing.T, d durableState) {
			if d.lastEvent != "model.response" {
				t.Fatalf("last event=%q, want model.response", d.lastEvent)
			}
			last := d.history[len(d.history)-1]
			if last.Role != "assistant" || len(last.ToolCalls) == 0 {
				t.Fatalf("last message=%+v, want assistant with tool calls", last)
			}
			for _, call := range last.ToolCalls {
				if d.toolResults[call.ID] {
					t.Fatalf("tool_results row exists for %s", call.ID)
				}
			}
		}},
		{crashpoint.BeforeToolCommit, "createRefund", func(t *testing.T, d durableState) {
			if d.refunds != 0 {
				t.Fatalf("refunds=%d, want 0", d.refunds)
			}
			if d.toolRequests[refundCall] {
				t.Fatalf("tool.request recorded for %s", refundCall)
			}
			if d.toolResults[refundCall] {
				t.Fatalf("tool_results row exists for %s", refundCall)
			}
		}},
		{crashpoint.AfterToolCommit, "createRefund", func(t *testing.T, d durableState) {
			if d.refunds != 1 {
				t.Fatalf("refunds=%d, want 1", d.refunds)
			}
			if !d.toolResults[refundCall] {
				t.Fatalf("no tool_results row for %s", refundCall)
			}
			last := d.history[len(d.history)-1]
			if last.Role != "tool" || last.CallID != refundCall {
				t.Fatalf("last message=%+v, want tool message for %s", last, refundCall)
			}
		}},
		{crashpoint.BeforeCompletion, "", func(t *testing.T, d durableState) {
			last := d.history[len(d.history)-1]
			if last.Role != "assistant" || len(last.ToolCalls) != 0 {
				t.Fatalf("last message=%+v, want assistant without tool calls", last)
			}
			if d.run.Status == "completed" {
				t.Fatal("status is completed")
			}
			if d.completedEvents != 0 {
				t.Fatalf("execution.completed events=%d, want 0", d.completedEvents)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(string(tc.point), func(t *testing.T) {
			ctx := context.Background()
			s, err := store.Open(t.TempDir() + "/world.db")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			world, err := s.Seed(ctx, 42, manifest.Digest)
			if err != nil {
				t.Fatal(err)
			}
			run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "task", "")
			if err != nil {
				t.Fatal(err)
			}
			var offered []crashpoint.Site
			hook := crashAt(tc.point, tc.operation, &offered)
			runner := Runner{
				Store:    s,
				Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest, Crash: hook},
				Manifest: manifest,
				Provider: ScriptedProvider{},
				Crash:    hook,
			}
			_, err = runner.Execute(ctx, run.ID, 20)
			if !errors.Is(err, crashpoint.ErrCrashed) {
				t.Fatalf("error=%v, want ErrCrashed", err)
			}
			state := readDurableState(t, ctx, s, run.ID, world.ID)
			if state.errorEvents != 0 {
				t.Fatalf("error events=%d, a crashed process records nothing", state.errorEvents)
			}
			if state.run.Status == "failed" {
				t.Fatal("run status is failed, a crashed process writes no status")
			}
			tc.check(t, state)
		})
	}
}

type durableState struct {
	run             store.Run
	history         []Message
	lastEvent       string
	requests        int
	responses       int
	errorEvents     int
	completedEvents int
	toolRequests    map[string]bool
	toolResults     map[string]bool
	refunds         int
}

func readDurableState(t *testing.T, ctx context.Context, s *store.Store, runID, worldID string) durableState {
	t.Helper()
	d := durableState{toolRequests: map[string]bool{}, toolResults: map[string]bool{}}
	var err error
	if d.run, err = s.Run(ctx, runID); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(d.run.Transcript), &d.history); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		d.lastEvent = e.Type
		switch e.Type {
		case "model.request":
			d.requests++
		case "model.response":
			d.responses++
		case "error":
			d.errorEvents++
		case "execution.completed":
			d.completedEvents++
		case "tool.request":
			var payload struct {
				CallID string `json:"call_id"`
			}
			if err = json.Unmarshal(e.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			d.toolRequests[payload.CallID] = true
		}
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT call_id FROM tool_results WHERE run_id=?", runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		d.toolResults[id] = true
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", worldID).Scan(&d.refunds); err != nil {
		t.Fatal(err)
	}
	return d
}
