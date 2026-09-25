package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"twinwright/internal/compiler"
)

func TestAnthropicToolRoundTrip(t *testing.T) {
	var sawResult bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "anthropic-key" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("headers key=%s version=%s", r.Header.Get("x-api-key"), r.Header.Get("anthropic-version"))
		}
		body, _ := io.ReadAll(r.Body)
		sawResult = strings.Contains(string(body), "tool_result")
		if !sawResult {
			w.Write([]byte(`{"content":[{"type":"tool_use","id":"toolu_1","name":"getCharge","input":{"id":"CH-1002"}}],"usage":{"input_tokens":5,"output_tokens":2}}`))
			return
		}
		if !strings.Contains(string(body), "toolu_1") {
			t.Errorf("raw tool use was not resent: %s", body)
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"Done"}]}`))
	}))
	defer server.Close()
	p := AnthropicProvider{APIKey: "anthropic-key", Model: "claude-test", URL: server.URL, Client: server.Client()}
	ops := []compiler.Operation{{ID: "getCharge", Required: []string{"id"}, Properties: map[string]string{"id": "string"}}}
	first, err := p.Next(context.Background(), "Inspect", nil, ops)
	if err != nil || len(first.ToolCalls) != 1 || first.ToolCalls[0].ID != "toolu_1" || first.Usage == nil || first.Usage.TotalTokens != 7 || len(first.RawOutput) != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := p.Next(context.Background(), "Inspect", []Message{first, {Role: "tool", CallID: "toolu_1", Content: `{"id":"CH-1002"}`}}, ops)
	if err != nil || second.Content != "Done" || !sawResult {
		t.Fatalf("second=%+v err=%v", second, err)
	}
}

func TestAnthropicCoalescesConsecutiveToolResults(t *testing.T) {
	var saw string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		saw = string(body)
		w.Write([]byte(`{"content":[{"type":"text","text":"Done"}]}`))
	}))
	defer server.Close()
	p := AnthropicProvider{APIKey: "anthropic-key", Model: "claude-test", URL: server.URL, Client: server.Client()}
	history := []Message{
		{Role: "assistant", RawOutput: []json.RawMessage{[]byte(`{"type":"tool_use","id":"toolu_a","name":"getCharge","input":{"id":"CH-1"}}`), []byte(`{"type":"tool_use","id":"toolu_b","name":"getCharge","input":{"id":"CH-2"}}`)}, ToolCalls: []ToolCall{{ID: "toolu_a", OperationID: "getCharge"}, {ID: "toolu_b", OperationID: "getCharge"}}},
		{Role: "tool", CallID: "toolu_a", Content: `{"id":"CH-1"}`, Status: 200},
		{Role: "tool", CallID: "toolu_b", Content: `{"error":"gone"}`, Status: 404},
	}
	if _, err := p.Next(context.Background(), "Inspect", history, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Count(saw, `"role":"user"`) != 2 {
		t.Fatalf("expected task user plus one coalesced tool user, got %s", saw)
	}
	if strings.Count(saw, `"type":"tool_result"`) != 2 || !strings.Contains(saw, `"tool_use_id":"toolu_a"`) || !strings.Contains(saw, `"tool_use_id":"toolu_b"`) {
		t.Fatalf("missing tool results: %s", saw)
	}
	if !strings.Contains(saw, `"is_error":true`) {
		t.Fatalf("expected is_error on failed tool: %s", saw)
	}
}

func TestAnthropicRedactsErrorsAndRequiresCredentials(t *testing.T) {
	if _, err := (AnthropicProvider{}).Next(context.Background(), "task", nil, nil); err == nil {
		t.Fatal("missing credentials accepted")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid x-api-key anthropic-secret-value"}}`))
	}))
	defer server.Close()
	p := AnthropicProvider{APIKey: "anthropic-secret-value", Model: "claude-test", URL: server.URL, Client: server.Client()}
	message, err := p.Next(context.Background(), "task", nil, nil)
	if err == nil || strings.Contains(err.Error(), "anthropic-secret-value") || !strings.Contains(message.RawBody, "[REDACTED]") {
		t.Fatalf("err=%v body=%s", err, message.RawBody)
	}
}
