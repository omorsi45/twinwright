package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"twinwright/internal/dispatch"
	"twinwright/internal/store"

	"twinwright/internal/compiler"
)

func TestOpenAIResponsesFunctionLoop(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("authorization missing")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("request JSON: %v", err)
		}
		if body["store"] != false {
			t.Errorf("expected store=false")
		}
		if requests == 1 {
			w.Write([]byte(`{"output":[{"type":"function_call","call_id":"call-1","name":"getCharge","arguments":"{\"id\":\"CH-1002\"}"}]}`))
		} else {
			input := body["input"].([]any)
			found := false
			for _, v := range input {
				m, ok := v.(map[string]any)
				if ok && m["type"] == "function_call_output" && m["call_id"] == "call-1" {
					found = true
				}
			}
			if !found {
				t.Errorf("prior tool output missing from stateless request")
			}
			w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"Done"}]}]}`))
		}
	}))
	defer server.Close()
	p := OpenAIProvider{APIKey: "test-key", Model: "test-model", URL: server.URL, Client: server.Client()}
	ops := []compiler.Operation{{ID: "getCharge", Required: []string{"id"}, Properties: map[string]string{"id": "string"}}}
	first, err := p.Next(context.Background(), "Inspect charge", nil, ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].OperationID != "getCharge" {
		t.Fatalf("first=%+v", first)
	}
	history := []Message{first, {Role: "tool", CallID: "call-1", OperationID: "getCharge", Status: 200, Content: `{"id":"CH-1002"}`}}
	second, err := p.Next(context.Background(), "Inspect charge", history, ops)
	if err != nil {
		t.Fatal(err)
	}
	if second.Content != "Done" || requests != 2 {
		t.Fatalf("second=%+v requests=%d", second, requests)
	}
}

func TestMalformedFunctionArgumentsRemainInRunLedger(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"output":[{"type":"function_call","call_id":"bad-call-42","name":"getCharge","arguments":"{bad"}]}`))
	}))
	defer server.Close()
	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	world, err := s.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "openai", "test-model", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	manifest := compiler.Manifest{Operations: []compiler.Operation{{ID: "getCharge", Required: []string{"id"}, Properties: map[string]string{"id": "string"}}}}
	provider := OpenAIProvider{APIKey: "test-key", Model: "test-model", URL: server.URL, Client: server.Client()}
	runner := Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: provider}
	if _, err = runner.Execute(ctx, run.ID, 1); err == nil {
		t.Fatal("expected malformed function arguments to fail")
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
		if e.Type == "model.response" && strings.Contains(string(e.Payload), "bad-call-42") && strings.Contains(string(e.Payload), "{bad") {
			found = true
		}
	}
	if !found {
		t.Fatalf("raw malformed call missing from ledger: %+v", events)
	}
}

func TestOpenAIInvalidJSONErrorIncludesResponseExcerpt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("not-json-response")) }))
	defer server.Close()
	p := OpenAIProvider{APIKey: "test-key", Model: "test-model", URL: server.URL, Client: server.Client()}
	_, err := p.Next(context.Background(), "task", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "not-json-response") {
		t.Fatalf("error=%v", err)
	}
}
