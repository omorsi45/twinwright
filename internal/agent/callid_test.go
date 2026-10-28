package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

// turnsProvider returns a fixed list of turns, one per Next call.
type turnsProvider struct {
	turns []Message
	next  int
}

func (p *turnsProvider) Next(context.Context, string, []Message, []compiler.Operation) (Message, error) {
	if p.next >= len(p.turns) {
		return Message{}, fmt.Errorf("stub provider has no turn %d", p.next+1)
	}
	turn := p.turns[p.next]
	p.next++
	return turn, nil
}

func assistantCalls(calls ...ToolCall) Message {
	return Message{Role: "assistant", ToolCalls: calls}
}

func TestProviderCallIDReuseFailsTheTurn(t *testing.T) {
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
	customer := map[string]any{"id": "C-104"}
	tests := []struct {
		name        string
		turns       []Message
		wantError   string
		wantResults int
		wantEvent   string
	}{
		{
			name: "across turns",
			turns: []Message{
				assistantCalls(ToolCall{ID: "dup", OperationID: "getCustomer", Arguments: customer}),
				assistantCalls(ToolCall{ID: "dup", OperationID: "listInvoices", Arguments: customer}),
			},
			wantError:   `reused call ID "dup"`,
			wantResults: 1,
			wantEvent:   "dup",
		},
		{
			name: "within one turn",
			turns: []Message{
				assistantCalls(
					ToolCall{ID: "twin", OperationID: "getCustomer", Arguments: customer},
					ToolCall{ID: "twin", OperationID: "listInvoices", Arguments: customer},
				),
			},
			wantError:   `call ID "twin" appears twice in one turn`,
			wantResults: 0,
			wantEvent:   "twin",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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
			run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "test", "test-model", "task", "")
			if err != nil {
				t.Fatal(err)
			}
			runner := Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: &turnsProvider{turns: tc.turns}}
			_, err = runner.Execute(ctx, run.ID, 10)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error=%v, want it to contain %s", err, tc.wantError)
			}
			saved, err := s.Run(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Status != "failed" {
				t.Fatalf("status=%s, want failed", saved.Status)
			}
			var results int
			if err = s.DB.QueryRow("SELECT count(*) FROM tool_results WHERE run_id=?", run.ID).Scan(&results); err != nil {
				t.Fatal(err)
			}
			if results != tc.wantResults {
				t.Fatalf("tool_results=%d, want %d", results, tc.wantResults)
			}
			events, err := s.Events(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			last := events[len(events)-1]
			if last.Type != "error" {
				t.Fatalf("last event=%q, want error", last.Type)
			}
			var payload struct {
				Message string `json:"message"`
			}
			if err = json.Unmarshal(last.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(payload.Message, tc.wantEvent) {
				t.Fatalf("error event message=%q, want it to name %q", payload.Message, tc.wantEvent)
			}
		})
	}
}
