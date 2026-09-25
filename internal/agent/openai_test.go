package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
