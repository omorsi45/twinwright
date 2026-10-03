package authz

import (
	"context"
	"testing"
)

// Screen exists so a caller that is not executing a run can ask what the policy
// would decide. Shadow comparison is the first such caller: it has proposed tool
// calls that were never dispatched under a policy, and it has to report the ones
// the policy would refuse.
//
// The risk a second entry point creates is drift. A parallel copy of the rules
// would keep passing its own tests while quietly disagreeing with the
// dispatcher, and a shadow report saying "the policy allows this" when the
// dispatcher would deny it is worse than no report. So the test that matters is
// not that Screen denies an ungranted permission on its own; it is that Screen
// and Decide return the same decision for the same call.
func TestScreenAgreesWithDecide(t *testing.T) {
	ctx := context.Background()
	policyYAML := "version: 1\nprincipal: {id: support}\npermissions: {allow: [customers.read, charges.read, refunds.create]}\nresources: {customer_ids: [C-104]}\nconstraints: {refund_max_cents: 1000}\n"
	r := newSecuredRun(t, policyYAML)
	policy, err := Parse([]byte(policyYAML), r.manifest)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		operation string
		args      map[string]any
	}{
		{"getCustomer", id("C-104")},  // allowed
		{"getCustomer", id("C-205")},  // customer scope, resolved from world state
		{"listInvoices", id("C-104")}, // permission not granted
		{"createRefund", map[string]any{"charge_id": "CH-1002", "amount_cents": 100, "reason": "x"}},  // allowed under the cap
		{"createRefund", map[string]any{"charge_id": "CH-1002", "amount_cents": 5000, "reason": "x"}}, // over the cap
		{"createRefund", map[string]any{"charge_id": "CH-1002", "amount_cents": 0, "reason": "x"}},    // arguments a handler rejects
	}

	call := 0
	for _, c := range cases {
		op := r.manifest.Operation(c.operation)
		if op == nil {
			t.Fatalf("unknown operation %s", c.operation)
		}
		// Screen is asked about the call number Decide is about to allocate.
		screened, err := Screen(ctx, r.store.DB, r.run.WorldID, policy, *op, c.args, call+1)
		if err != nil {
			t.Fatal(err)
		}
		decided := r.decide(t, c.operation, c.args)
		if screened != decided {
			t.Errorf("%s %v: screen=%+v decide=%+v", c.operation, c.args, screened, decided)
		}
		// An invalid call consumes no call number in the dispatcher, so the
		// screen must not assume one either.
		if !decided.Invalid {
			call++
		}
	}
}

// A screen that consumed a call number would shift every later decision by one,
// which a policy with a temporary grant turns into a wrong verdict rather than
// an off-by-one in a field nobody reads.
func TestScreenConsumesNoCallNumber(t *testing.T) {
	ctx := context.Background()
	policyYAML := "version: 1\nprincipal: {id: support}\npermissions: {allow: [charges.read]}\ntemporary_grants: [{permission: customers.read, starts_at_call: 1, ends_at_call: 1}]\n"
	r := newSecuredRun(t, policyYAML)
	policy, err := Parse([]byte(policyYAML), r.manifest)
	if err != nil {
		t.Fatal(err)
	}
	op := r.manifest.Operation("getCustomer")
	if op == nil {
		t.Fatal("unknown operation getCustomer")
	}
	for i := 0; i < 5; i++ {
		screened, err := Screen(ctx, r.store.DB, r.run.WorldID, policy, *op, id("C-104"), 1)
		if err != nil {
			t.Fatal(err)
		}
		if !screened.Allowed || screened.Call != 1 {
			t.Fatalf("screen %d: %+v", i, screened)
		}
	}
	if first := r.decide(t, "getCustomer", id("C-104")); !first.Allowed || first.Call != 1 {
		t.Fatalf("five screens moved the run's call counter: %+v", first)
	}
	if second := r.decide(t, "getCustomer", id("C-104")); second.Allowed || second.Reason != ReasonNotGranted || second.Call != 2 {
		t.Fatalf("the grant window is not being applied: %+v", second)
	}
}
