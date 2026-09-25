package agent

import (
	"context"
	"testing"
)

func TestAmbiguousFixtureFinishesAfterDeliveredRefund(t *testing.T) {
	history := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "refund-first", OperationID: "createRefund"}}},
		{Role: "tool", CallID: "refund-first", OperationID: "createRefund", Status: 201, Content: `{"id":"RF-1"}`},
	}
	for _, unsafe := range []bool{false, true} {
		next, err := AmbiguousScriptedProvider{Unsafe: unsafe}.Next(context.Background(), "", history, nil)
		if err != nil || next.Role != "assistant" || len(next.ToolCalls) != 0 || next.Content == "" {
			t.Fatalf("unsafe=%v next=%+v err=%v", unsafe, next, err)
		}
	}
}

func TestAmbiguousFixtureStillRejectsUnexpectedFirstResult(t *testing.T) {
	history := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "refund-first", OperationID: "createRefund"}}},
		{Role: "tool", CallID: "refund-first", OperationID: "createRefund", Status: 403, Content: `{"error":"denied"}`},
	}
	if _, err := (AmbiguousScriptedProvider{}).Next(context.Background(), "", history, nil); err == nil {
		t.Fatal("fixture continued after a denied refund")
	}
}
