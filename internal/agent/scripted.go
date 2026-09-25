package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"twinwright/internal/compiler"
)

// ScriptedProvider is a deterministic example fixture. It is not a model provider.
type ScriptedProvider struct{}

func (ScriptedProvider) Next(_ context.Context, _ string, history []Message, _ []compiler.Operation) (Message, error) {
	tools := []Message{}
	for _, m := range history {
		if m.Role == "tool" {
			tools = append(tools, m)
		}
	}
	call := func(id, op string, args map[string]any) Message {
		return Message{Role: "assistant", ToolCalls: []ToolCall{{ID: id, OperationID: op, Arguments: args}}}
	}
	switch len(tools) {
	case 0:
		return call("fixture-1", "getCustomer", map[string]any{"id": "C-104"}), nil
	case 1:
		return call("fixture-2", "listInvoices", map[string]any{"id": "C-104"}), nil
	case 2:
		return call("fixture-3", "listCharges", map[string]any{"id": "INV-104"}), nil
	case 3:
		if tools[2].Status == 503 {
			return call("fixture-4", "listCharges", map[string]any{"id": "INV-104"}), nil
		}
	}
	last := tools[len(tools)-1]
	if last.OperationID == "listCharges" && last.Status == 200 {
		var charges []struct {
			ID          string `json:"id"`
			AmountCents int64  `json:"amount_cents"`
		}
		if err := json.Unmarshal([]byte(last.Content), &charges); err != nil {
			return Message{}, err
		}
		if len(charges) != 2 {
			return Message{}, fmt.Errorf("expected two charges on example invoice")
		}
		return call("fixture-refund", "createRefund", map[string]any{"charge_id": charges[1].ID, "amount_cents": charges[1].AmountCents, "reason": "duplicate charge"}), nil
	}
	if last.OperationID == "createRefund" && last.Status == 201 {
		return Message{Role: "assistant", Content: "Duplicate charge refunded."}, nil
	}
	return Message{}, fmt.Errorf("fixture cannot continue after %s HTTP %d", last.OperationID, last.Status)
}
