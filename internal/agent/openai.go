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
)

type OpenAIProvider struct {
	APIKey string
	Model  string
	URL    string
	Client *http.Client
}

func (p OpenAIProvider) Next(ctx context.Context, task string, history []Message, ops []compiler.Operation) (Message, error) {
	if p.APIKey == "" || p.Model == "" {
		return Message{}, fmt.Errorf("OpenAI API key and model are required")
	}
	endpoint := p.URL
	if endpoint == "" {
		endpoint = "https://api.openai.com/v1/responses"
	}
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	input := []any{
		map[string]any{"role": "developer", "content": "You are testing a fictional billing service. Use tools to investigate. Refund only a justified duplicate charge. A 503 is temporary; retry if needed. Never claim success without checking tool results."},
		map[string]any{"role": "user", "content": task},
	}
	for _, m := range history {
		switch m.Role {
		case "assistant":
			if len(m.RawOutput) > 0 {
				for _, raw := range m.RawOutput {
					input = append(input, raw)
				}
			} else if m.Content != "" {
				input = append(input, map[string]any{"role": "assistant", "content": m.Content})
			}
		case "tool":
			output, _ := json.Marshal(map[string]any{"status": m.Status, "body": json.RawMessage(m.Content)})
			input = append(input, map[string]any{"type": "function_call_output", "call_id": m.CallID, "output": string(output)})
		}
	}
	tools := []any{}
	for _, op := range ops {
		properties := map[string]any{}
		for name, typ := range op.Properties {
			properties[name] = map[string]any{"type": typ}
		}
		tools = append(tools, map[string]any{"type": "function", "name": op.ID, "description": description(op.ID), "parameters": map[string]any{"type": "object", "properties": properties, "required": op.Required, "additionalProperties": false}, "strict": len(op.Required) == len(op.Properties)})
	}
	payload := map[string]any{"model": p.Model, "store": false, "parallel_tool_calls": false, "include": []string{"reasoning.encrypted_content"}, "input": input, "tools": tools}
	data, err := json.Marshal(payload)
	if err != nil {
		return Message{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
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
		return Message{}, fmt.Errorf("OpenAI response HTTP %d: %s", res.StatusCode, string(body))
	}
	var wire struct {
		Output []json.RawMessage `json:"output"`
	}
	if err = json.Unmarshal(body, &wire); err != nil {
		return Message{}, fmt.Errorf("decode OpenAI response: %w; body=%q", err, string(body[:min(len(body), 1024)]))
	}
	message := Message{Role: "assistant", RawOutput: wire.Output}
	var texts []string
	for _, raw := range wire.Output {
		var item struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err = json.Unmarshal(raw, &item); err != nil {
			return Message{}, err
		}
		switch item.Type {
		case "function_call":
			var args map[string]any
			decoder := json.NewDecoder(strings.NewReader(item.Arguments))
			decoder.UseNumber()
			if err = decoder.Decode(&args); err != nil {
				return Message{}, fmt.Errorf("invalid arguments for %s: %w", item.Name, err)
			}
			message.ToolCalls = append(message.ToolCalls, ToolCall{ID: item.CallID, OperationID: item.Name, Arguments: args})
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					texts = append(texts, part.Text)
				}
			}
		}
	}
	message.Content = strings.Join(texts, "\n")
	if len(message.ToolCalls) == 0 && message.Content == "" {
		return Message{}, fmt.Errorf("OpenAI response contained no text or tool calls")
	}
	return message, nil
}

func description(id string) string {
	switch id {
	case "getCustomer":
		return "Get a customer by ID."
	case "listInvoices":
		return "List invoices for a customer ID."
	case "listCharges":
		return "List charges for an invoice ID, including amounts and refund totals."
	case "getCharge":
		return "Get a charge by ID and its refunded amount."
	case "createRefund":
		return "Create a refund for a charge. Use only after confirming a duplicate; amount_cents must be the intended refund amount."
	default:
		return id
	}
}
