package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/eval"
	"twinwright/internal/store"
)

type scenarioProvider struct{}

func (scenarioProvider) Next(_ context.Context, _ string, history []Message, _ []compiler.Operation) (Message, error) {
	tools := []Message{}
	for _, m := range history {
		if m.Role == "tool" {
			tools = append(tools, m)
		}
	}
	call := func(id, op string, args map[string]any) Message {
		return Message{Role: "assistant", ToolCalls: []ToolCall{{ID: id, OperationID: op, Arguments: args}}}
	}
	switch len(tools) {
	case 0:
		return call("c1", "getCustomer", map[string]any{"id": "C-104"}), nil
	case 1:
		return call("c2", "listInvoices", map[string]any{"id": "C-104"}), nil
	case 2:
		return call("c3", "listCharges", map[string]any{"id": "INV-104"}), nil
	case 3:
		return call("c4", "listCharges", map[string]any{"id": "INV-104"}), nil
	case 4:
		var charges []struct {
			ID          string `json:"id"`
			AmountCents int64  `json:"amount_cents"`
		}
		if err := json.Unmarshal([]byte(tools[3].Content), &charges); err != nil {
			return Message{}, err
		}
		return call("c5", "createRefund", map[string]any{"charge_id": charges[1].ID, "amount_cents": charges[1].AmountCents, "reason": "duplicate charge"}), nil
	default:
		return Message{Role: "assistant", Content: "The duplicate charge was refunded."}, nil
	}
}

func TestPauseReopenResumeWithoutDuplicateRefund(t *testing.T) {
	ctx := context.Background()
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
	path := t.TempDir() + "/world.db"
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, w.ID, "duplicate-charge", "scripted", "Customer C-104 was charged twice. Refund only the duplicate.", "listCharges")
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: scenarioProvider{}}
	paused, err := runner.Execute(ctx, run.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != "paused" {
		t.Fatalf("status=%s", paused.Status)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	runner.Store = s
	runner.Dispatch.Store = s
	completed, err := runner.Execute(ctx, run.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "completed" {
		t.Fatalf("status=%s", completed.Status)
	}
	report, err := eval.DuplicateCharge(ctx, s, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Fatalf("evaluation=%+v", report)
	}
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var mutations, errors int
	for _, e := range events {
		if e.Type == "state.mutation" {
			mutations++
		}
		if e.Type == "error" {
			errors++
		}
	}
	if mutations != 1 || errors != 1 {
		t.Fatalf("mutations=%d errors=%d", mutations, errors)
	}
}
