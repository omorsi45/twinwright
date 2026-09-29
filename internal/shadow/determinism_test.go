package shadow

import (
	"encoding/json"
	"testing"
)

// Compare builds its residual lists by walking maps keyed on operation and
// canonical arguments. Go randomises map iteration order deliberately, so a
// comparison assembled that way is reproducible only by luck.
//
// That matters more here than it would elsewhere: a shadow report is evidence
// about an agent's behaviour, and evidence that reorders itself between two runs
// over identical inputs cannot be diffed, committed as a fixture, or cited. The
// rest of the runtime already treats this as a rule rather than a nicety, which
// is why compiler, authz, bench, chaos, metrics, trace and fork all sort before
// returning.
//
// Repeating the comparison is what exposes it: a single call always looks fine.
func TestCompareIsDeterministic(t *testing.T) {
	// Several distinct operations on each side, plus a shared one, so the maps
	// hold enough keys for ordering to vary.
	proposed := []ProposedAction{
		{CallID: "1", OperationID: "billing.listCharges", Arguments: map[string]any{"customer_id": "C-104"}},
		{CallID: "2", OperationID: "billing.refundCharge", Arguments: map[string]any{"charge_id": "CH-2"}},
		{CallID: "3", OperationID: "crm.getCustomer", Arguments: map[string]any{"customer_id": "C-104"}},
		{CallID: "4", OperationID: "ticketing.createTicket", Arguments: map[string]any{"subject": "duplicate charge"}},
		{CallID: "5", OperationID: "messaging.postMessage", Arguments: map[string]any{"channel": "support"}},
	}
	observed := []Observation{
		{Kind: "human_action", OperationID: "crm.getCustomer", Arguments: map[string]any{"customer_id": "C-104"}},
		{Kind: "human_action", OperationID: "billing.listCharges", Arguments: map[string]any{"customer_id": "C-104"}},
		{Kind: "human_action", OperationID: "billing.refundCharge", Arguments: map[string]any{"charge_id": "CH-9"}},
		{Kind: "external_event", OperationID: "crm.addNote", Arguments: map[string]any{"customer_id": "C-104"}},
	}

	first, err := Compare(proposed, observed)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}

	// 200 repetitions: with this many distinct keys the chance that a
	// randomised walk reproduces one fixed order every time is negligible, so a
	// pass here is a real property and not a lucky seed.
	for i := 0; i < 200; i++ {
		got, err := Compare(proposed, observed)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != string(want) {
			t.Fatalf("comparison %d differs from the first run over identical input\nfirst: %s\ngot:   %s", i, want, encoded)
		}
	}
}
