package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

type failAfterRefundProvider struct{}

func (failAfterRefundProvider) Next(ctx context.Context, task string, history []Message, ops []compiler.Operation) (Message, error) {
	for _, m := range history {
		if m.Role == "tool" && m.OperationID == "createRefund" {
			return Message{}, errors.New("simulated provider failure after refund")
		}
	}
	return scenarioProvider{}.Next(ctx, task, history, ops)
}

func TestProviderFailureAfterRefundCanResumeWithoutSecondMutation(t *testing.T) {
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
	world, err := s.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "test", "test-model", "task", "listCharges")
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: failAfterRefundProvider{}}
	if _, err = runner.Execute(ctx, run.ID, 10); err == nil {
		t.Fatal("expected provider failure")
	}
	failed, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" {
		t.Fatalf("status=%s", failed.Status)
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
	runner.Provider = scenarioProvider{}
	completed, err := runner.Execute(ctx, run.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "completed" {
		t.Fatalf("status=%s", completed.Status)
	}
	report, err := eval.DuplicateCharge(ctx, s, world.ID)
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
	mutations := 0
	for _, e := range events {
		if e.Type == "state.mutation" {
			mutations++
		}
	}
	if mutations != 1 {
		t.Fatalf("mutations=%d", mutations)
	}
}

func TestDispatchFailureIsAuditedAfterRollback(t *testing.T) {
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
	s, err := store.Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err := s.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, w.ID, "duplicate-charge", "scripted", "fixture-v1", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`CREATE TRIGGER fail_refund_result BEFORE INSERT ON tool_results WHEN NEW.operation_id='createRefund' BEGIN SELECT RAISE(ABORT, 'injected storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	runner := Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: scenarioProvider{}}
	if _, err = runner.Execute(ctx, run.ID, 10); err == nil {
		t.Fatal("expected dispatch failure")
	}
	saved, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "failed" {
		t.Fatalf("status=%s", saved.Status)
	}
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type == "error" {
			found = true
		}
	}
	if !found {
		t.Fatal("dispatch failure absent from ledger")
	}
	var count int
	if err = s.DB.QueryRow("SELECT count(*) FROM refunds WHERE world_id=?", w.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("refund committed despite failed transaction: %d", count)
	}
}

type invalidProvider struct {
	s     *store.Store
	runID string
	t     *testing.T
}

func (p invalidProvider) Next(ctx context.Context, _ string, _ []Message, _ []compiler.Operation) (Message, error) {
	events, err := p.s.Events(ctx, p.runID)
	if err != nil {
		p.t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type == "model.request" {
			found = true
		}
	}
	if !found {
		p.t.Error("model request was not durable before provider call")
	}
	return Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "bad-call", OperationID: "notCompiled", Arguments: map[string]any{}}}}, nil
}

func TestInvalidProviderOutputIsRecordedAndRunFails(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err := s.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, w.ID, "duplicate-charge", "test", "test-model", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: compiler.Manifest{}}, Manifest: compiler.Manifest{}, Provider: invalidProvider{s: s, runID: run.ID, t: t}}
	_, err = runner.Execute(ctx, run.ID, 1)
	if err == nil || !strings.Contains(err.Error(), "invalid tool call") {
		t.Fatalf("error=%v", err)
	}
	saved, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "failed" {
		t.Fatalf("status=%s", saved.Status)
	}
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var request, response, failure bool
	for _, e := range events {
		switch e.Type {
		case "model.request":
			request = true
		case "model.response":
			response = true
		case "error":
			failure = true
		}
	}
	if !request || !response || !failure {
		t.Fatalf("missing model audit events: %+v", events)
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
	run, err := s.CreateRun(ctx, w.ID, "duplicate-charge", "scripted", "fixture-v1", "Customer C-104 was charged twice. Refund only the duplicate.", "listCharges")
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: scenarioProvider{}}
	paused, err := runner.Execute(ctx, run.ID, 5)
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
