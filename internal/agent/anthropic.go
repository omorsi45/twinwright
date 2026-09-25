package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"twinwright/internal/compiler"
	"twinwright/internal/redact"
)

// AnthropicProvider calls the Anthropic Messages API.
type AnthropicProvider struct {
	APIKey string
	Model  string
	URL    string
	Client *http.Client
}

func (p AnthropicProvider) Next(ctx context.Context, task string, history []Message, ops []compiler.Operation) (Message, error) {
	if p.APIKey == "" || p.Model == "" {
		return Message{}, fmt.Errorf("Anthropic API key and model are required")
	}
	endpoint := p.URL
	if endpoint == "" {
		endpoint = "https://api.anthropic.com/v1/messages"
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	messages := []any{map[string]any{"role": "user", "content": task}}
	for _, m := range history {
		switch m.Role {
		case "assistant":
			if len(m.RawOutput) > 0 {
				messages = append(messages, map[string]any{"role": "assistant", "content": m.RawOutput})
				continue
			}
			content := []any{}
			if m.Content != "" {
				content = append(content, map[string]any{"type": "text", "text": m.Content})
			}
			for _, call := range m.ToolCalls {
				content = append(content, map[string]any{"type": "tool_use", "id": call.ID, "name": call.OperationID, "input": call.Arguments})
			}
			if len(content) > 0 {
				messages = append(messages, map[string]any{"role": "assistant", "content": content})
			}
		case "tool":
			messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": m.CallID, "content": m.Content}}})
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
		tools = append(tools, map[string]any{"name": op.ID, "description": description(key), "input_schema": map[string]any{"type": "object", "properties": properties, "required": op.Required}})
	}
	payload := map[string]any{"model": p.Model, "max_tokens": 4096, "system": guidanceFor(ops), "messages": messages}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return Message{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("x-api-key", p.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
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
		return Message{RawBody: text}, fmt.Errorf("Anthropic HTTP %d: %s", res.StatusCode, text)
	}
	var wire struct {
		Content []json.RawMessage `json:"content"`
		Usage   *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err = json.Unmarshal(body, &wire); err != nil {
		text := redact.String(string(body), p.APIKey)
		return Message{RawBody: text}, fmt.Errorf("decode Anthropic response: %w", err)
	}
	out := Message{Role: "assistant", RawOutput: wire.Content}
	var texts []string
	for _, raw := range wire.Content {
		var block struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err = json.Unmarshal(raw, &block); err != nil {
			return out, err
		}
		switch block.Type {
		case "text":
			texts = append(texts, block.Text)
		case "tool_use":
			args := map[string]any{}
			decoder := json.NewDecoder(bytes.NewReader(block.Input))
			decoder.UseNumber()
			if err = decoder.Decode(&args); err != nil {
				return out, fmt.Errorf("invalid arguments for %s: %w", block.Name, err)
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: block.ID, OperationID: block.Name, Arguments: args})
		}
	}
	out.Content = joinLines(texts)
	if wire.Usage != nil {
		out.Usage = &Usage{InputTokens: wire.Usage.InputTokens, OutputTokens: wire.Usage.OutputTokens, TotalTokens: wire.Usage.InputTokens + wire.Usage.OutputTokens}
	}
	if out.Content == "" && len(out.ToolCalls) == 0 {
		return out, fmt.Errorf("Anthropic response contained no text or tool calls")
	}
	return out, nil
}

func joinLines(lines []string) string {
	out := ""
	for i, line := range lines {
		if i > 0 {
			out += "\n"
		}
		out += line
	}
	return out
}
