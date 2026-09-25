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
	guidance := guidanceFor(ops)
	input := []any{
		map[string]any{"role": "developer", "content": guidance},
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
		descriptionKey := op.Behavior
		if descriptionKey == "" {
			descriptionKey = op.ID
		}
		tools = append(tools, map[string]any{"type": "function", "name": op.ID, "description": description(descriptionKey), "parameters": map[string]any{"type": "object", "properties": properties, "required": op.Required, "additionalProperties": false}, "strict": len(op.Required) == len(op.Properties)})
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
		text := redact.String(string(body), p.APIKey)
		return Message{RawBody: text}, fmt.Errorf("OpenAI response HTTP %d: %s", res.StatusCode, text)
	}
	var wire struct {
		Output []json.RawMessage `json:"output"`
		Usage  *Usage            `json:"usage"`
	}
	if err = json.Unmarshal(body, &wire); err != nil {
		text := redact.String(string(body), p.APIKey)
		return Message{RawBody: text}, fmt.Errorf("decode OpenAI response: %w; body=%q", err, text[:min(len(text), 1024)])
	}
	message := Message{Role: "assistant", RawOutput: wire.Output, Usage: wire.Usage}
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
			return message, err
		}
		switch item.Type {
		case "function_call":
			var args map[string]any
			decoder := json.NewDecoder(strings.NewReader(item.Arguments))
			decoder.UseNumber()
			if err = decoder.Decode(&args); err != nil {
				return message, fmt.Errorf("invalid arguments for %s: %w", item.Name, err)
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
		return message, fmt.Errorf("OpenAI response contained no text or tool calls")
	}
	return message, nil
}

func description(id string) string {
	switch id {
	case "getCustomer", "billing.getCustomer":
		return "Get a customer by ID."
	case "listInvoices", "billing.listInvoices":
		return "List invoices for a customer ID."
	case "listCharges", "billing.listCharges":
		return "List charges for an invoice ID, including amounts and refund totals."
	case "getCharge", "billing.getCharge":
		return "Get a charge by ID and its refunded amount."
	case "createRefund", "billing.createRefund":
		return "Create a refund for a charge. Use only after confirming a duplicate; amount_cents must be the intended refund amount."
	case "getSubscription", "billing.getSubscription":
		return "Get a subscription by ID, including customer ID, plan, and status."
	case "crmGetAccount", "crm.getAccount":
		return "Get a CRM account by ID, including customer linkage, contacts, status, and notes. Incident evidence may be in notes."
	case "crmSearchAccounts", "crm.searchAccounts":
		return "Search CRM accounts by ID, customer ID, status, representative, or customer name."
	case "crmAddAccountNote", "crm.addAccountNote":
		return "Add a nonempty note to a CRM account documenting the investigation and outcome."
	case "crmUpdateAccountStatus", "crm.updateAccountStatus":
		return "Update a CRM account. Valid statuses are active, needs_followup, and resolved. Use needs_followup for an active engineering incident, resolved otherwise."
	case "ticketCreateIssue", "ticket.createIssue":
		return "Create an engineering issue in project PROJ-ENG for account A-104 when incident evidence exists. Valid priorities are low, medium, and high."
	case "ticketGetIssue", "ticket.getIssue":
		return "Get a ticketing issue by ID, including its current status and comments."
	case "ticketSearchIssues", "ticket.searchIssues":
		return "Search ticketing issues by ID, project, account, title, status, or priority."
	case "ticketAddComment", "ticket.addComment":
		return "Add a comment to a ticketing issue by issue ID."
	case "ticketTransitionIssue", "ticket.transitionIssue":
		return "Transition an issue from open to in_progress, then from in_progress to resolved."
	case "messageListChannels", "messaging.listChannels":
		return "List channels in workspace WS-1 to discover the support channel."
	case "messageReadChannel", "messaging.readChannel":
		return "Read a channel by ID, including its current messages."
	case "messagePostMessage", "messaging.postMessage":
		return "Post a message to the support channel when incident evidence exists. Include the affected account or customer ID."
	default:
		return id
	}
}
