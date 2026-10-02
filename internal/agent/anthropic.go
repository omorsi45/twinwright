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
	// Timeout is the per-attempt deadline. Zero means DefaultCallTimeout.
	Timeout time.Duration
	// MaxOutputTokens is the required max_tokens the API takes. Zero means
	// DefaultMaxOutputTokens. It is configurable because it is the ceiling a
	// truncated completion hit, and raising it is the documented remedy.
	MaxOutputTokens int
}

// DefaultMaxOutputTokens is the ceiling used when nothing configured one. The
// Messages API has no default of its own: max_tokens is a required parameter.
const DefaultMaxOutputTokens = 4096

func (p AnthropicProvider) Next(ctx context.Context, task string, history []Message, ops []compiler.Operation) (Message, error) {
	if p.APIKey == "" || p.Model == "" {
		return Message{}, fmt.Errorf("Anthropic API key and model are required")
	}
	endpoint := p.URL
	if endpoint == "" {
		endpoint = "https://api.anthropic.com/v1/messages"
	}
	deadline := callDeadline(p.Timeout)
	attemptCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	client := p.Client
	if client == nil {
		client = &http.Client{}
	}
	messages := []any{map[string]any{"role": "user", "content": task}}
	for i := 0; i < len(history); i++ {
		m := history[i]
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
			results := []any{}
			for i < len(history) && history[i].Role == "tool" {
				block := map[string]any{"type": "tool_result", "tool_use_id": history[i].CallID, "content": history[i].Content}
				if history[i].Status == 0 || history[i].Status >= 400 {
					block["is_error"] = true
				}
				results = append(results, block)
				i++
			}
			i--
			messages = append(messages, map[string]any{"role": "user", "content": results})
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
	maxTokens := p.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxOutputTokens
	}
	payload := map[string]any{"model": p.Model, "max_tokens": maxTokens, "system": guidanceFor(ops), "messages": messages}
	if len(tools) > 0 {
		payload["tools"] = tools
		payload["tool_choice"] = map[string]any{"type": "auto", "disable_parallel_tool_use": true}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return Message{}, err
	}
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("x-api-key", p.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return Message{}, interrupted(ctx, attemptCtx, deadline, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return Message{}, interrupted(ctx, attemptCtx, deadline, err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		text := redact.String(string(body), p.APIKey)
		return Message{RawBody: text}, fmt.Errorf("Anthropic HTTP %d: %s", res.StatusCode, text)
	}
	var wire struct {
		Content []json.RawMessage `json:"content"`
		// stop_reason carries the truncation signal on this API. max_tokens is
		// the caller's ceiling; model_context_window_exceeded is the model's own
		// window filling up, which raising max_tokens cannot fix.
		StopReason string `json:"stop_reason"`
		Usage      *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err = json.Unmarshal(body, &wire); err != nil {
		text := redact.String(string(body), p.APIKey)
		return Message{RawBody: text}, fmt.Errorf("decode Anthropic response: %w", err)
	}
	if wire.StopReason == "max_tokens" || wire.StopReason == "model_context_window_exceeded" {
		return Message{RawBody: redact.String(string(body), p.APIKey)}, truncatedCompletion(wire.StopReason)
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
