package agent

import (
	"context"
	"testing"

	"twinwright/internal/compiler"
)

func TestCompanyFixtureUsesBoundBehaviorWithRenamedOperation(t *testing.T) {
	provider := CompanyScriptedProvider{Scenario: "company-routine"}
	ops := []compiler.Operation{{ID: "lookupCustomer", Behavior: "billing.getCustomer"}}
	message, err := provider.Next(context.Background(), "task", nil, ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].OperationID != "lookupCustomer" {
		t.Fatalf("first call=%+v", message)
	}
}

func TestCompanyFixtureRetriesResolvedOperationID(t *testing.T) {
	provider := CompanyScriptedProvider{Scenario: "company-routine"}
	ops := []compiler.Operation{
		{ID: "crmGetAccount", Behavior: "billing.getCustomer"},
		{ID: "lookupAccount", Behavior: "crm.getAccount"},
	}
	history := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", OperationID: "crmGetAccount", Arguments: map[string]any{"id": "C-104"}}}},
		{Role: "tool", CallID: "call-1", OperationID: "crmGetAccount", Status: 503},
	}
	message, err := provider.Next(context.Background(), "task", history, ops)
	if err != nil {
		t.Fatal(err)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].OperationID != "crmGetAccount" {
		t.Fatalf("retry=%+v", message)
	}
}
