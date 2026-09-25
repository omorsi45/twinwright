package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"twinwright/internal/compiler"
)

// AmbiguousScriptedProvider demonstrates recovery choices without a model call.
type AmbiguousScriptedProvider struct{ Unsafe bool }

func (p AmbiguousScriptedProvider) Next(_ context.Context, _ string, history []Message, _ []compiler.Operation) (Message, error) {
	var tools []Message
	for _, message := range history {
		if message.Role == "tool" {
			tools = append(tools, message)
		}
	}
	call := func(id, operation string, args map[string]any) Message {
		return Message{Role: "assistant", ToolCalls: []ToolCall{{ID: id, OperationID: operation, Arguments: args}}}
	}
	refundArgs := map[string]any{"charge_id": "CH-1002", "amount_cents": 500, "reason": "duplicate charge partial refund"}
	switch len(tools) {
	case 0:
		return call("refund-first", "createRefund", refundArgs), nil
	case 1:
		if tools[0].OperationID != "createRefund" || tools[0].Status != 0 {
			return Message{}, fmt.Errorf("expected lost refund response")
		}
		if p.Unsafe {
			return call("refund-unsafe-retry", "createRefund", refundArgs), nil
		}
		return call("refund-reconcile", "getCharge", map[string]any{"id": "CH-1002"}), nil
	case 2:
		if p.Unsafe {
			if tools[1].Status != 201 {
				return Message{}, fmt.Errorf("unsafe retry did not complete")
			}
			return Message{Role: "assistant", Content: "Retried the refund."}, nil
		}
		var charge struct {
			RefundedCents int64 `json:"refunded_cents"`
		}
		if tools[1].Status != 200 || json.Unmarshal([]byte(tools[1].Content), &charge) != nil || charge.RefundedCents != 500 {
			return Message{}, fmt.Errorf("reconciliation did not confirm the refund")
		}
		return Message{Role: "assistant", Content: "Confirmed the refund after the timeout."}, nil
	}
	return Message{}, fmt.Errorf("ambiguous fixture received excess tool results")
}
