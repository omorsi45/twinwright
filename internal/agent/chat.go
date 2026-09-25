package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"twinwright/internal/compiler"
	"twinwright/internal/redact"
)

// ChatCompletionsProvider talks to an OpenAI-compatible /chat/completions endpoint.
// BaseURL is the server origin plus optional prefix, such as http://127.0.0.1:11434/v1.
type ChatCompletionsProvider struct {
	APIKey  string
	Model   string
	BaseURL string
	Client  *http.Client
}

func (p ChatCompletionsProvider) Next(ctx context.Context, task string, history []Message, ops []compiler.Operation) (Message, error) {
	if p.Model == "" || p.BaseURL == "" {
		return Message{}, fmt.Errorf("chat completions model and base URL are required")
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	messages := []any{map[string]any{"role": "system", "content": guidanceFor(ops)}, map[string]any{"role": "user", "content": task}}
	for _, m := range history {
		switch m.Role {
		case "assistant":
			if len(m.ToolCalls) > 0 {
				calls := make([]any, 0, len(m.ToolCalls))
				for _, call := range m.ToolCalls {
					args, err := json.Marshal(call.Arguments)
					if err != nil {
						return Message{}, err
					}
					calls = append(calls, map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.OperationID, "arguments": string(args)}})
				}
				var content any
				if m.Content != "" {
					content = m.Content
				}
				messages = append(messages, map[string]any{"role": "assistant", "content": content, "tool_calls": calls})
			} else if m.Content != "" {
				messages = append(messages, map[string]any{"role": "assistant", "content": m.Content})
			}
		case "tool":
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": m.CallID, "content": m.Content})
		}
	}
	tools := []any{}
	for _, op := range ops {
		properties := map[string]any{}
		for name, typ := range op.Properties {
			properties[name] = map[string]any{"type": typ}
		}
		key := op.Behavior
		if key == "" {
			key = op.ID
		}
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name": op.ID, "description": description(key),
			"parameters": map[string]any{"type": "object", "properties": properties, "required": op.Required},
		}})
	}
	payload := map[string]any{"model": p.Model, "messages": messages}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return Message{}, err
	}
	endpoint := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return Message{}, err
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return Message{}, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return Message{}, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		text := redact.String(string(body), p.APIKey)
		return Message{RawBody: text}, fmt.Errorf("chat completions HTTP %d: %s", res.StatusCode, text)
	}
	var wire struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err = json.Unmarshal(body, &wire); err != nil || len(wire.Choices) == 0 {
		text := redact.String(string(body), p.APIKey)
		return Message{RawBody: text}, fmt.Errorf("decode chat completions response: %v", err)
	}
	var message struct {
		Content   *string `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err = json.Unmarshal(wire.Choices[0].Message, &message); err != nil {
		return Message{RawBody: redact.String(string(body), p.APIKey)}, err
	}
	out := Message{Role: "assistant", RawOutput: []json.RawMessage{wire.Choices[0].Message}}
	if message.Content != nil {
		out.Content = *message.Content
	}
	for _, call := range message.ToolCalls {
		args := map[string]any{}
		decoder := json.NewDecoder(strings.NewReader(call.Function.Arguments))
		decoder.UseNumber()
		if err = decoder.Decode(&args); err != nil {
			return out, fmt.Errorf("invalid arguments for %s: %w", call.Function.Name, err)
		}
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: call.ID, OperationID: call.Function.Name, Arguments: args})
	}
	if wire.Usage != nil {
		out.Usage = &Usage{InputTokens: wire.Usage.PromptTokens, OutputTokens: wire.Usage.CompletionTokens, TotalTokens: wire.Usage.TotalTokens}
	}
	if out.Content == "" && len(out.ToolCalls) == 0 {
		return out, fmt.Errorf("chat completions response contained no text or tool calls")
	}
	return out, nil
}
