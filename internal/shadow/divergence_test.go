package shadow

import (
	"encoding/json"
	"testing"
)

// renderValue renders a divergence field value the way the comparison does, so an
// assertion does not depend on whether a number arrived as int, float64 or
// json.Number.
func renderValue(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The comparison used to key an action on its operation and the whole of its
// marshalled arguments, so a refund of 500 against an observed refund of 5905 on
// the same charge came back as two unrelated entries: one in only_proposed, one
// in only_observed. Both halves are useless on their own. The reader has to
// notice they are the same charge, and nothing in the report says so.
func TestCompareReportsOneDivergenceRatherThanTwoHalves(t *testing.T) {
	proposed := []ProposedAction{
		{CallID: "1", OperationID: "getCustomer", Arguments: map[string]any{"id": "C-104"}},
		{CallID: "2", OperationID: "createRefund", Arguments: map[string]any{"charge_id": "CH-1002", "amount_cents": 500, "reason": "duplicate charge"}},
	}
	observed := []Observation{
		{Kind: "human_action", At: "2026-09-25T12:00:00Z", OperationID: "getCustomer", Arguments: map[string]any{"id": "C-104"}},
		{Kind: "human_action", At: "2026-09-25T12:02:00Z", OperationID: "createRefund", Arguments: map[string]any{"charge_id": "CH-1002", "amount_cents": 5905, "reason": "duplicate charge"}},
	}
	cmp, err := Compare(proposed, observed)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmp.OnlyProposed) != 0 || len(cmp.OnlyObserved) != 0 {
		t.Fatalf("the diverging pair is still reported as two unrelated halves: %+v", cmp)
	}
	if len(cmp.Arguments) != 1 {
		t.Fatalf("want one argument divergence, got %+v", cmp.Arguments)
	}
	got := cmp.Arguments[0]
	if got.OperationID != "createRefund" || got.Resource != "charge_id=CH-1002" {
		t.Fatalf("divergence is not attributed to the charge: %+v", got)
	}
	if len(got.Fields) != 1 {
		t.Fatalf("only amount_cents differs, got %+v", got.Fields)
	}
	field := got.Fields[0]
	if field.Field != "amount_cents" || !field.ProposedPresent || !field.ObservedPresent {
		t.Fatalf("field divergence: %+v", field)
	}
	if renderValue(t, field.Proposed) != "500" || renderValue(t, field.Observed) != "5905" {
		t.Fatalf("both values must be reported: %+v", field)
	}
	if len(cmp.Timing.OrderDivergences) != 0 {
		t.Fatalf("the two streams are in the same order: %+v", cmp.Timing)
	}
}

// Pairing on operation alone would call these the same action and report a
// divergence on charge_id, which reads as "the agent refunded a different
// amount" when it actually refunded a different charge. The pair has to agree on
// the resource before any argument is compared.
func TestCompareLeavesDifferentResourcesUnpaired(t *testing.T) {
	cmp, err := Compare(
		[]ProposedAction{{CallID: "1", OperationID: "createRefund", Arguments: map[string]any{"charge_id": "CH-2", "amount_cents": 100, "reason": "x"}}},
		[]Observation{{Kind: "human_action", OperationID: "createRefund", Arguments: map[string]any{"charge_id": "CH-9", "amount_cents": 100, "reason": "x"}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmp.Arguments) != 0 {
		t.Fatalf("two different charges were paired: %+v", cmp.Arguments)
	}
	if len(cmp.OnlyProposed) != 1 || len(cmp.OnlyObserved) != 1 {
		t.Fatalf("%+v", cmp)
	}
}

// Identical actions in a different order are not the same run. The old
// comparison counted them as matched and said nothing, because its key carried
// no position.
func TestCompareReportsOrderDivergenceForAReorderedStream(t *testing.T) {
	refund := map[string]any{"charge_id": "CH-1002", "amount_cents": 5905, "reason": "duplicate charge"}
	proposed := []ProposedAction{
		{CallID: "1", OperationID: "getCustomer", Arguments: map[string]any{"id": "C-104"}},
		{CallID: "2", OperationID: "createRefund", Arguments: refund},
	}
	observed := []Observation{
		{Kind: "human_action", At: "2026-09-25T12:00:00Z", OperationID: "createRefund", Arguments: refund},
		{Kind: "human_action", At: "2026-09-25T12:02:00Z", OperationID: "getCustomer", Arguments: map[string]any{"id": "C-104"}},
	}
	cmp, err := Compare(proposed, observed)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmp.Arguments) != 0 {
		t.Fatalf("nothing about the arguments diverged: %+v", cmp.Arguments)
	}
	if len(cmp.Matched) != 2 || len(cmp.OnlyProposed) != 0 || len(cmp.OnlyObserved) != 0 {
		t.Fatalf("%+v", cmp)
	}
	if len(cmp.Timing.OrderDivergences) != 1 {
		t.Fatalf("want one order divergence, got %+v", cmp.Timing)
	}
	order := cmp.Timing.OrderDivergences[0]
	if order.ProposedEarlier != "getCustomer(id=C-104)" || order.ObservedEarlier != "createRefund(charge_id=CH-1002)" {
		t.Fatalf("order divergence does not name both sides: %+v", order)
	}
	if cmp.Timing.PairedActions != 2 || cmp.Timing.ObservedTimestamps != 2 {
		t.Fatalf("timing denominators: %+v", cmp.Timing)
	}
}

// Timing has to carry its denominator. An observed stream whose records have no
// timestamp supports no claim about timing, and reporting zero would read as
// "measured, and nothing diverged".
func TestCompareCountsObservationsThatCarriedNoTimestamp(t *testing.T) {
	cmp, err := Compare(
		[]ProposedAction{{CallID: "1", OperationID: "getCustomer", Arguments: map[string]any{"id": "C-104"}}},
		[]Observation{{Kind: "human_action", OperationID: "getCustomer", Arguments: map[string]any{"id": "C-104"}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Timing.ObservedTimestamps != 0 || cmp.Timing.PairedActions != 1 {
		t.Fatalf("%+v", cmp.Timing)
	}
	if cmp.Timing.ElapsedGap == "" {
		t.Fatal("an unmeasured elapsed gap must say so rather than be absent from the report")
	}
}

// A comparison run without a policy has measured nothing about authorization.
// An empty refusal list with no evaluated flag reads exactly like a clean
// screen, which is the failure this project has been bitten by twice.
func TestComparePolicyAbsenceIsNotAnEmptyRefusalList(t *testing.T) {
	cmp, err := Compare(
		[]ProposedAction{{CallID: "1", OperationID: "getCustomer", Arguments: map[string]any{"id": "C-104"}}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Policy.Evaluated {
		t.Fatalf("no policy was supplied: %+v", cmp.Policy)
	}
	if cmp.Policy.Absent == "" {
		t.Fatalf("the report must say why there is no policy verdict: %+v", cmp.Policy)
	}
	encoded, err := json.Marshal(cmp.Policy)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == `{"evaluated":false}` {
		t.Fatalf("policy report carries no reason for the absence: %s", encoded)
	}
}

// An action the policy refuses is a finding even when a human did the same
// thing: the observed stream records what someone with other permissions did,
// not what this principal is allowed to do.
func TestComparePolicyRefusalSurvivesAnIdenticalObservedAction(t *testing.T) {
	refund := map[string]any{"charge_id": "CH-1002", "amount_cents": 5905, "reason": "duplicate charge"}
	screen := &PolicyScreen{
		Principal: "support-agent-1",
		Screened:  1,
		Refusals: []PolicyRefusal{{
			OperationID: "createRefund", Arguments: refund,
			Permission: "refunds.create", Reason: "permission_not_granted", Call: 1,
		}},
	}
	cmp, err := CompareWith(
		[]ProposedAction{{CallID: "1", OperationID: "createRefund", Arguments: refund}},
		[]Observation{{Kind: "human_action", At: "2026-09-25T12:02:00Z", OperationID: "createRefund", Arguments: refund}},
		CompareOptions{Policy: screen},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !cmp.Policy.Evaluated || cmp.Policy.Principal != "support-agent-1" || cmp.Policy.Screened != 1 {
		t.Fatalf("%+v", cmp.Policy)
	}
	if len(cmp.Policy.Refused) != 1 {
		t.Fatalf("the refusal was dropped because the action was also observed: %+v", cmp.Policy)
	}
	if !cmp.Policy.Refused[0].AlsoObserved {
		t.Fatalf("the report must say the refused action appears in the observed stream: %+v", cmp.Policy.Refused[0])
	}
	if cmp.Policy.Refused[0].Resource != "charge_id=CH-1002" {
		t.Fatalf("the refusal must name the resource it addresses: %+v", cmp.Policy.Refused[0])
	}
	if len(cmp.Matched) != 1 {
		t.Fatalf("the action still matches an observed one: %+v", cmp)
	}
}
