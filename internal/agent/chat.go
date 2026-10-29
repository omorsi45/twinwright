package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
	// Timeout is the per-attempt deadline. Zero means DefaultCallTimeout.
	Timeout time.Duration
	// MaxOutputTokens is the output ceiling sent with each request. Zero sends
	// none and leaves the server's own limit in force.
	MaxOutputTokens int
}

// chatCompletionsURL is the endpoint for a base URL: the path /chat/completions
// appended to the base's own path, with its query kept. Appending to the whole
// string put the suffix inside the query, so a base such as
// http://host/v1?api-version=1 was sent to /v1. The error is url.Parse's own.
func chatCompletionsURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	const suffix = "/chat/completions"
	u.Path = strings.TrimRight(u.Path, "/") + suffix
	if u.RawPath != "" {
		u.RawPath = strings.TrimRight(u.RawPath, "/") + suffix
	}
	return u.String(), nil
}

func (p ChatCompletionsProvider) Next(ctx context.Context, task string, history []Message, ops []compiler.Operation) (Message, error) {
	if p.Model == "" || p.BaseURL == "" {
		return Message{}, fmt.Errorf("chat completions model and base URL are required")
	}
	deadline := callDeadline(p.Timeout)
	attemptCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	client := p.Client
	if client == nil {
		client = &http.Client{}
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
	if p.MaxOutputTokens > 0 {
		payload["max_tokens"] = p.MaxOutputTokens
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return Message{}, err
	}
	endpoint, err := chatCompletionsURL(p.BaseURL)
	if err != nil {
		return Message{}, safeRequestError(err, p.BaseURL, p.APIKey)
	}
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return Message{}, safeRequestError(err, endpoint, p.APIKey)
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return Message{}, requestFailed(ctx, attemptCtx, deadline, safeTransportError(err, p.APIKey))
	}
	defer res.Body.Close()
	body, err := readBody(res)
	if err != nil {
		return Message{}, interrupted(ctx, attemptCtx, deadline, err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		text := redact.String(string(body), p.APIKey)
		failure := fmt.Errorf("chat completions HTTP %d: %s", res.StatusCode, text)
		if interrupt := statusInterruption(res, failure); interrupt != nil {
			return Message{RawBody: text}, interrupt
		}
		return Message{RawBody: text}, failure
	}
	var wire struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message json.RawMessage `json:"message"`
			// finish_reason carries the truncation signal here. "length" is
			// OpenAI's value for the request ceiling; "model_length" is the
			// second value Mistral's own SDK defines for the model's own limit.
			FinishReason string `json:"finish_reason"`
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
	if reason := wire.Choices[0].FinishReason; reason == "length" || reason == "model_length" {
		return Message{RawBody: redact.String(string(body), p.APIKey)}, truncatedCompletion(reason)
	}
	raw := redactOutput([]json.RawMessage{wire.Choices[0].Message}, p.APIKey)
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
	if err = json.Unmarshal(raw[0], &message); err != nil {
		return Message{RawBody: redact.String(string(body), p.APIKey)}, err
	}
	out := Message{Role: "assistant", RawOutput: raw, ResponseID: redact.Secrets(wire.ID, p.APIKey), ServerModel: redact.Secrets(wire.Model, p.APIKey), FinishReason: redact.Secrets(wire.Choices[0].FinishReason, p.APIKey), HTTPStatus: res.StatusCode}
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
