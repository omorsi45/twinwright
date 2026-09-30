package agent

import (
	"context"
	"encoding/json"
	"testing"
)

// The concurrent-mutation bench case injects a partial refund on the duplicate
// charge while the agent is still listing charges. The amount the agent then
// asks for was correct when it read the invoice and is stale by the time it
// writes, so billing answers 409 "refund exceeds remaining charge".
//
// A hard stop there measures nothing: the case reports status error, and §8's
// concurrent-state-mutation coverage becomes a crash rather than a result. The
// behaviour a correct agent must show is to re-read the charge and refund only
// what is still outstanding, so the customer ends up whole and never
// over-refunded.
//
// These tests drive the fixture as a pure function of history, which is what it
// is, so the assertions are exact rather than probabilistic.

func toolMsg(op string, status int, content string) Message {
	return Message{Role: "tool", OperationID: op, Status: status, Content: content}
}

// twoCharges is the invoice the billing world ships: a 5905-cent charge billed
// twice, the second being the duplicate to refund.
const twoCharges = `[{"id":"CH-1001","amount_cents":5905},{"id":"CH-1002","amount_cents":5905}]`

func historyThroughRefund(status int, body string) []Message {
	return []Message{
		toolMsg("getCustomer", 200, `{"id":"C-104"}`),
		toolMsg("listInvoices", 200, `[{"id":"INV-104"}]`),
		toolMsg("listCharges", 200, twoCharges),
		toolMsg("createRefund", status, body),
	}
}

func TestFixtureReconcilesAfterConflictingRefund(t *testing.T) {
	history := historyThroughRefund(409, `{"error":"refund exceeds remaining charge"}`)
	msg, err := ScriptedProvider{}.Next(context.Background(), "", history, nil)
	if err != nil {
		t.Fatalf("fixture stopped on 409 instead of reconciling: %v", err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("want one tool call, got %d", len(msg.ToolCalls))
	}
	got := msg.ToolCalls[0]
	if got.OperationID != "getCharge" {
		t.Fatalf("after a 409 the fixture must re-read the charge, got %q", got.OperationID)
	}
	if got.Arguments["id"] != "CH-1002" {
		t.Fatalf("must re-read the duplicate charge, got %v", got.Arguments["id"])
	}
}

// The remainder is computed from what the service reports, not from the
// difference the agent assumed. Refunding the full amount again would exceed the
// charge; refunding nothing would leave the customer partly charged for a
// duplicate.
func TestFixtureRefundsOnlyTheOutstandingRemainder(t *testing.T) {
	history := append(historyThroughRefund(409, `{"error":"refund exceeds remaining charge"}`),
		toolMsg("getCharge", 200, `{"id":"CH-1002","invoice_id":"INV-104","amount_cents":5905,"refunded_cents":500}`))
	msg, err := ScriptedProvider{}.Next(context.Background(), "", history, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("want one tool call, got %d", len(msg.ToolCalls))
	}
	got := msg.ToolCalls[0]
	if got.OperationID != "createRefund" {
		t.Fatalf("want createRefund, got %q", got.OperationID)
	}
	amount, err := json.Marshal(got.Arguments["amount_cents"])
	if err != nil {
		t.Fatal(err)
	}
	if string(amount) != "5405" {
		t.Fatalf("want the 5405-cent remainder, got %s", amount)
	}
	if got.Arguments["charge_id"] != "CH-1002" {
		t.Fatalf("want the duplicate charge, got %v", got.Arguments["charge_id"])
	}
}

// A charge already fully refunded by the concurrent mutation needs no second
// write at all. Issuing a zero refund would be a pointless side effect, and
// issuing another full one would double-refund, which is the exact defect this
// benchmark exists to detect.
func TestFixtureIssuesNoRefundWhenAlreadyWhole(t *testing.T) {
	history := append(historyThroughRefund(409, `{"error":"refund exceeds remaining charge"}`),
		toolMsg("getCharge", 200, `{"id":"CH-1002","invoice_id":"INV-104","amount_cents":5905,"refunded_cents":5905}`))
	msg, err := ScriptedProvider{}.Next(context.Background(), "", history, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(msg.ToolCalls) != 0 {
		t.Fatalf("a fully refunded charge needs no write, got %q", msg.ToolCalls[0].OperationID)
	}
	if msg.Content == "" {
		t.Fatal("want a closing message explaining the charge was already refunded")
	}
}

// The reconciled refund still terminates the run rather than looping.
func TestFixtureCompletesAfterReconciledRefund(t *testing.T) {
	history := append(historyThroughRefund(409, `{"error":"refund exceeds remaining charge"}`),
		toolMsg("getCharge", 200, `{"id":"CH-1002","invoice_id":"INV-104","amount_cents":5905,"refunded_cents":500}`),
		toolMsg("createRefund", 201, `{"id":"RF-1","charge_id":"CH-1002","amount_cents":5405}`))
	msg, err := ScriptedProvider{}.Next(context.Background(), "", history, nil)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(msg.ToolCalls) != 0 {
		t.Fatalf("want completion, got a call to %q", msg.ToolCalls[0].OperationID)
	}
	if msg.Content == "" {
		t.Fatal("want a closing message")
	}
}

// An unrelated failure must still stop the fixture. Reconciliation is a response
// to one specific, diagnosable conflict, not a general retry that would mask
// every error the service reports.
func TestFixtureStillStopsOnUnrelatedFailure(t *testing.T) {
	history := historyThroughRefund(500, `{"error":"boom"}`)
	_, err := ScriptedProvider{}.Next(context.Background(), "", history, nil)
	if err == nil {
		t.Fatal("a 500 must remain a hard stop")
	}
}
