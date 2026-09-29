package agent

import (
	"context"
	"testing"
)

// A lost write response is the hardest case the company scenario can face, and
// the one the flagship demo has to show: the refund may or may not have
// committed, and the agent cannot tell from the response it never received.
//
// The company fixture previously treated any non-2xx status as fatal, so a
// timeout-after-commit ended the run instead of recovering from it. Retrying
// blindly would refund a customer twice; giving up would leave an incident
// half-handled. The correct move is to reconcile against the charge first.
func TestCompanyFixtureReconcilesAfterALostRefundResponse(t *testing.T) {
	provider := CompanyScriptedProvider{Scenario: "company-incident"}
	history := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "company-refund", OperationID: "createRefund",
			Arguments: map[string]any{"charge_id": "CH-1002", "amount_cents": 5905, "reason": "duplicate charge"},
		}}},
		// Status 0 is the runtime's lost-response signal: the request went out,
		// no response came back, and the effect may already be committed.
		{Role: "tool", CallID: "company-refund", OperationID: "createRefund", Status: 0},
	}
	message, err := provider.Next(context.Background(), "task", history, nil)
	if err != nil {
		t.Fatalf("a lost refund response must be recoverable, got error: %v", err)
	}
	if len(message.ToolCalls) != 1 {
		t.Fatalf("expected one reconciliation call, got %+v", message)
	}
	call := message.ToolCalls[0]
	if call.OperationID != "getCharge" {
		t.Fatalf("expected the charge to be read back before deciding, got %s", call.OperationID)
	}
	if call.Arguments["id"] != "CH-1002" {
		t.Fatalf("reconciliation must read the charge that was refunded, got %v", call.Arguments["id"])
	}
}

// companyPrefix is the successful investigation that precedes the refund: two
// equal charges on one invoice, and a CRM note naming a retry worker so the
// incident branch is live.
func companyPrefix() []Message {
	return []Message{
		{Role: "tool", CallID: "c1", OperationID: "getCustomer", Status: 200, Content: `{"id":"C-104","name":"Alex Chen"}`},
		{Role: "tool", CallID: "c2", OperationID: "getSubscription", Status: 200, Content: `{"id":"SUB-104"}`},
		{Role: "tool", CallID: "c3", OperationID: "crmGetAccount", Status: 200,
			Content: `{"id":"A-104","notes":[{"body":"billing retry worker double-posted a charge"}]}`},
		{Role: "tool", CallID: "c4", OperationID: "listInvoices", Status: 200, Content: `[{"id":"INV-104"}]`},
		{Role: "tool", CallID: "c5", OperationID: "listCharges", Status: 200,
			Content: `[{"id":"CH-1001","amount_cents":5905},{"id":"CH-1002","amount_cents":5905}]`},
	}
}

// Once the charge shows the money already went back, refunding again would be a
// duplicate side effect caused by the recovery itself. This is the invariant the
// whole ambiguous-commit story exists to protect, so it gets its own test.
func TestCompanyFixtureDoesNotRefundTwiceAfterReconciliationConfirms(t *testing.T) {
	provider := CompanyScriptedProvider{Scenario: "company-incident"}
	history := append(companyPrefix(),
		Message{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "company-refund", OperationID: "createRefund",
			Arguments: map[string]any{"charge_id": "CH-1002", "amount_cents": 5905, "reason": "duplicate charge"},
		}}},
		Message{Role: "tool", CallID: "company-refund", OperationID: "createRefund", Status: 0},
		Message{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "company-reconcile", OperationID: "getCharge",
			Arguments: map[string]any{"id": "CH-1002"},
		}}},
		// The refund did land. The response was what got lost, not the effect.
		Message{Role: "tool", CallID: "company-reconcile", OperationID: "getCharge", Status: 200,
			Content: `{"id":"CH-1002","amount_cents":5905,"refunded_cents":5905}`},
	)
	message, err := provider.Next(context.Background(), "task", history, nil)
	if err != nil {
		t.Fatalf("confirmed reconciliation must let the run continue, got error: %v", err)
	}
	if len(message.ToolCalls) != 1 {
		t.Fatalf("expected the workflow to continue with one call, got %+v", message)
	}
	if got := message.ToolCalls[0].OperationID; got == "createRefund" {
		t.Fatal("the refund was reissued after reconciliation confirmed it had committed")
	} else if got != "crmAddAccountNote" {
		t.Fatalf("expected the CRM note to follow a confirmed refund, got %s", got)
	}
}

// The mirror case: the response was lost because the write never happened. Here
// retrying is correct, and declining to retry would leave the customer unrefunded
// while the run reports success.
func TestCompanyFixtureRetriesWhenReconciliationShowsNoRefund(t *testing.T) {
	provider := CompanyScriptedProvider{Scenario: "company-incident"}
	history := append(companyPrefix(),
		Message{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "company-refund", OperationID: "createRefund",
			Arguments: map[string]any{"charge_id": "CH-1002", "amount_cents": 5905, "reason": "duplicate charge"},
		}}},
		Message{Role: "tool", CallID: "company-refund", OperationID: "createRefund", Status: 0},
		Message{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "company-reconcile", OperationID: "getCharge",
			Arguments: map[string]any{"id": "CH-1002"},
		}}},
		Message{Role: "tool", CallID: "company-reconcile", OperationID: "getCharge", Status: 200,
			Content: `{"id":"CH-1002","amount_cents":5905,"refunded_cents":0}`},
	)
	message, err := provider.Next(context.Background(), "task", history, nil)
	if err != nil {
		t.Fatalf("an unconfirmed refund must be retryable, got error: %v", err)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].OperationID != "createRefund" {
		t.Fatalf("expected the refund to be retried, got %+v", message)
	}
	if message.ToolCalls[0].Arguments["charge_id"] != "CH-1002" {
		t.Fatalf("retry must target the same charge, got %v", message.ToolCalls[0].Arguments)
	}
}
