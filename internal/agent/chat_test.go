package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"twinwright/internal/compiler"
)

func TestChatCompletionsToolRoundTrip(t *testing.T) {
	var sawTool, sawAuth bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		text := string(body)
		sawTool = strings.Contains(text, `"role":"tool"`)
		sawAuth = r.Header.Get("Authorization") == "Bearer local-key"
		if !sawTool {
			w.Write([]byte(`{"choices":[{"message":{"content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"getCharge","arguments":"{\"id\":\"CH-1002\"}"}}]}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"Done"}}]}`))
	}))
	defer server.Close()
	p := ChatCompletionsProvider{APIKey: "local-key", Model: "local", BaseURL: server.URL + "/v1", Client: server.Client()}
	ops := []compiler.Operation{{ID: "getCharge", Required: []string{"id"}, Properties: map[string]string{"id": "string"}}}
	first, err := p.Next(context.Background(), "Inspect", nil, ops)
	if err != nil || len(first.ToolCalls) != 1 || first.ToolCalls[0].OperationID != "getCharge" || first.Usage == nil || first.Usage.TotalTokens != 7 || len(first.RawOutput) != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := p.Next(context.Background(), "Inspect", []Message{first, {Role: "tool", CallID: "call-1", Content: `{"id":"CH-1002"}`}}, ops)
	if err != nil || second.Content != "Done" || second.Usage != nil || !sawTool || !sawAuth {
		t.Fatalf("second=%+v tool=%v auth=%v err=%v", second, sawTool, sawAuth, err)
	}
}

func TestChatCompletionsToolAssistantUsesNullContent(t *testing.T) {
	var saw string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		saw = string(body)
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()
	p := ChatCompletionsProvider{Model: "local", BaseURL: server.URL + "/v1", Client: server.Client()}
	history := []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", OperationID: "getCharge", Arguments: map[string]any{"id": "CH-1"}}}}, {Role: "tool", CallID: "call-1", Content: `{}`}}
	if _, err := p.Next(context.Background(), "Inspect", history, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(saw, `"content":null`) || strings.Contains(saw, `"content":""`) {
		t.Fatalf("tool assistant content should be null: %s", saw)
	}
}

func TestChatCompletionsRedactsErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"bad key local-secret-value"}`))
	}))
	defer server.Close()
	p := ChatCompletionsProvider{APIKey: "local-secret-value", Model: "local", BaseURL: server.URL + "/v1", Client: server.Client()}
	message, err := p.Next(context.Background(), "task", nil, nil)
	if err == nil || strings.Contains(err.Error(), "local-secret-value") || !strings.Contains(message.RawBody, "[REDACTED]") {
		t.Fatalf("err=%v body=%s", err, message.RawBody)
	}
}

func TestChatCompletionsRequiresEndpoint(t *testing.T) {
	if _, err := (ChatCompletionsProvider{Model: "local"}).Next(context.Background(), "task", nil, nil); err == nil {
		t.Fatal("missing base URL accepted")
	}
}
