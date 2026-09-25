package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"twinwright/internal/authz"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

type exposureProvider struct {
	turns []Message
	seen  [][]string
}

func operationIDs(ops []compiler.Operation) []string {
	var ids []string
	for _, op := range ops {
		ids = append(ids, op.ID)
	}
	sort.Strings(ids)
	return ids
}

func (p *exposureProvider) Next(_ context.Context, _ string, _ []Message, ops []compiler.Operation) (Message, error) {
	p.seen = append(p.seen, operationIDs(ops))
	return p.turns[len(p.seen)-1], nil
}

func TestRunnerExposesOnlyActiveOperationsAndDeniesHiddenCalls(t *testing.T) {
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
	policy, err := authz.Parse([]byte("version: 1\nprincipal: {id: support}\npermissions: {allow: [customers.read, charges.read]}\ntemporary_grants: [{permission: invoices.read, starts_at_call: 2, ends_at_call: 2}]\n"), manifest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := policy.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	world, err := s.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRunConfigured(ctx, world.ID, "duplicate-charge", "test", "test-model", "task", store.RunOptions{AuthJSON: encoded, AuthDigest: policy.Digest()})
	if err != nil {
		t.Fatal(err)
	}
	call := func(id, op string, args map[string]any) Message {
		return Message{Role: "assistant", ToolCalls: []ToolCall{{ID: id, OperationID: op, Arguments: args}}}
	}
	provider := &exposureProvider{turns: []Message{
		call("c1", "getCustomer", map[string]any{"id": "C-104"}),
		call("c2", "listInvoices", map[string]any{"id": "C-104"}),
		call("c3", "createRefund", map[string]any{"charge_id": "CH-1002", "amount_cents": 100, "reason": "duplicate"}),
		{Role: "assistant", Content: "done"},
	}}
	runner := Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: provider}
	if _, err := runner.Execute(ctx, run.ID, 10); err != nil {
		t.Fatal(err)
	}
	base := "getCharge,getCustomer,listCharges"
	want := []string{base, "getCharge,getCustomer,listCharges,listInvoices", base, base}
	if len(provider.seen) != len(want) {
		t.Fatalf("provider turns=%d", len(provider.seen))
	}
	for i := range want {
		if got := strings.Join(provider.seen[i], ","); got != want[i] {
			t.Errorf("turn %d exposed %s, want %s", i+1, got, want[i])
		}
	}
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	for _, event := range events {
		if event.Type != "model.request" {
			continue
		}
		var request struct {
			Operations []compiler.Operation `json:"operations"`
		}
		if err := json.Unmarshal(event.Payload, &request); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(operationIDs(request.Operations), ","); got != want[requests] {
			t.Errorf("model request %d recorded %s, want %s", requests+1, got, want[requests])
		}
		requests++
	}
	var status, refunds int
	if err := s.DB.QueryRow("SELECT status FROM tool_results WHERE run_id=? AND call_id='c3'", run.ID).Scan(&status); err != nil || status != 403 {
		t.Fatalf("hidden call status=%d err=%v", status, err)
	}
	if err := s.DB.QueryRow("SELECT count(*) FROM refunds").Scan(&refunds); err != nil || refunds != 0 {
		t.Fatalf("hidden call wrote %d refunds err=%v", refunds, err)
	}
}
