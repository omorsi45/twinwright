package agent

import (
	"context"
	"testing"
)

// Before the flagship agent moves a customer's money it reads the charge it is
// about to refund. That is the careful behaviour on its own terms: the invoice
// listing is a snapshot from earlier in the investigation, and something else may
// have refunded the charge since. Refunding without looking is how a second
// refund gets issued against a charge that was already made whole.
//
// It also makes a fault the flagship needs reachable. A stale read can only be
// injected where the runtime holds a snapshot of an identical earlier call, so
// with reconciliation as the only getCharge in the trajectory there was nothing
// to go stale. With an inspection read before the write, a stale read during
// reconciliation reports the refund as missing and the agent issues a second
// one, which is the failure the flagship's counterfactual step explains.

func TestCompanyFixtureInspectsTheChargeBeforeRefunding(t *testing.T) {
	provider := CompanyScriptedProvider{Scenario: "company-incident"}
	message, err := provider.Next(context.Background(), "task", companyPrefix(), nil)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if len(message.ToolCalls) != 1 {
		t.Fatalf("want one call, got %+v", message)
	}
	call := message.ToolCalls[0]
	if call.OperationID != "getCharge" {
		t.Fatalf("the duplicate must be read before it is refunded, got %s", call.OperationID)
	}
	if call.Arguments["id"] != "CH-1002" {
		t.Fatalf("must inspect the duplicate charge, got %v", call.Arguments["id"])
	}
}

// Having looked and found the money still outstanding, the refund goes ahead for
// the full duplicated amount.
func TestCompanyFixtureRefundsAfterInspectionShowsNothingRefunded(t *testing.T) {
	provider := CompanyScriptedProvider{Scenario: "company-incident"}
	history := append(companyPrefix(),
		Message{Role: "tool", CallID: "c6", OperationID: "getCharge", Status: 200,
			Content: `{"id":"CH-1002","invoice_id":"INV-104","amount_cents":5905,"refunded_cents":0}`})
	message, err := provider.Next(context.Background(), "task", history, nil)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if len(message.ToolCalls) != 1 {
		t.Fatalf("want one call, got %+v", message)
	}
	call := message.ToolCalls[0]
	if call.OperationID != "createRefund" {
		t.Fatalf("want createRefund after a clean inspection, got %s", call.OperationID)
	}
	if call.Arguments["charge_id"] != "CH-1002" {
		t.Fatalf("want the duplicate charge, got %v", call.Arguments["charge_id"])
	}
}

// And having looked and found the charge already whole, no refund is issued at
// all. This is the safety property the inspection buys: whoever refunded it
// first, a second refund would be money out the door twice.
func TestCompanyFixtureSkipsTheRefundWhenInspectionShowsItAlreadyRefunded(t *testing.T) {
	provider := CompanyScriptedProvider{Scenario: "company-incident"}
	history := append(companyPrefix(),
		Message{Role: "tool", CallID: "c6", OperationID: "getCharge", Status: 200,
			Content: `{"id":"CH-1002","invoice_id":"INV-104","amount_cents":5905,"refunded_cents":5905}`})
	message, err := provider.Next(context.Background(), "task", history, nil)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if len(message.ToolCalls) != 1 {
		t.Fatalf("want one call, got %+v", message)
	}
	if got := message.ToolCalls[0].OperationID; got == "createRefund" {
		t.Fatal("an already-refunded charge must not be refunded again")
	}
}
