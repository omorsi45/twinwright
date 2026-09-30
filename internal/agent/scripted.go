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
	// A 409 means the amount was correct when the invoice was read and is stale
	// by the time it was written: something else refunded part of this charge in
	// between. Retrying the same amount would fail identically and refunding
	// blind could exceed the charge, so the only safe move is to re-read the
	// charge and let the service's own figures decide what is still outstanding.
	if last.OperationID == "createRefund" && last.Status == 409 {
		duplicate, err := duplicateChargeID(tools)
		if err != nil {
			return Message{}, err
		}
		return call("fixture-recheck", "getCharge", map[string]any{"id": duplicate}), nil
	}
	if last.OperationID == "getCharge" && last.Status == 200 {
		var charge struct {
			ID            string `json:"id"`
			AmountCents   int64  `json:"amount_cents"`
			RefundedCents int64  `json:"refunded_cents"`
		}
		if err := json.Unmarshal([]byte(last.Content), &charge); err != nil {
			return Message{}, err
		}
		remaining := charge.AmountCents - charge.RefundedCents
		if remaining <= 0 {
			// Already whole. A zero refund is a side effect with no purpose and
			// another full one would double-refund, which is the defect this
			// scenario exists to detect.
			return Message{Role: "assistant", Content: "Duplicate charge was already fully refunded; no further refund issued."}, nil
		}
		return call("fixture-refund-remainder", "createRefund", map[string]any{"charge_id": charge.ID, "amount_cents": remaining, "reason": "duplicate charge remainder"}), nil
	}
	return Message{}, fmt.Errorf("fixture cannot continue after %s HTTP %d", last.OperationID, last.Status)
}

// duplicateChargeID recovers the duplicate charge from the most recent successful
// listCharges rather than from the failed call's own arguments, so the identifier
// always comes from something the service actually returned.
func duplicateChargeID(tools []Message) (string, error) {
	for i := len(tools) - 1; i >= 0; i-- {
		if tools[i].OperationID != "listCharges" || tools[i].Status != 200 {
			continue
		}
		var charges []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(tools[i].Content), &charges); err != nil {
			return "", err
		}
		if len(charges) != 2 {
			return "", fmt.Errorf("expected two charges on example invoice")
		}
		return charges[1].ID, nil
	}
	return "", fmt.Errorf("no successful listCharges to reconcile against")
}
