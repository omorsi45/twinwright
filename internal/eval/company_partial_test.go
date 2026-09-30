package eval

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/behavior"
	"twinwright/internal/chaos"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

// A 409 says the amount was too large, not that nothing is owed.
//
// This test exists because the previous commit got that wrong. Reading the
// charge before refunding it made a stale read reachable, the poisoned read
// provoked a second full refund, and billing refused it with 409 because a
// refund may not exceed what the charge still owes. Treating that rejection as
// proof the money was already back was correct for that trajectory, where the
// charge was fully refunded, and it is wrong in general.
//
// Billing returns 409 whenever the request exceeds amount_cents minus
// refunded_cents. A charge refunded by 500 of 5905 refuses a 5905 request just
// as loudly as a fully refunded one does, and 5405 is still owed. So the
// shortcut turns a partial refund by another writer into a customer who is
// quietly short, with the agent reporting the incident resolved.
//
// The fix is the one the billing fixture already uses: a refusal means re-read
// and refund only the remainder. What makes this worth a test rather than a
// patch is that the failure is invisible from the agent's own transcript. Every
// call it made succeeded or was answered, it never refunded twice, and it never
// exceeded the charge. Only the world's final state shows the shortfall, which
// is exactly the class of bug this project exists to catch.
func TestPartialRefundByAnotherWriterStillMakesTheCustomerWhole(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join("..", "..", "examples", "company")
	definition, err := os.ReadFile(filepath.Join(root, "world.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := compiler.CompileWorld(definition, func(path string) ([]byte, error) {
		return os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	}, behavior.Builtin())
	if err != nil {
		t.Fatal(err)
	}

	// One rule is enough here, and no stale read is needed. Another writer
	// refunds 500 of the 5905 duplicate while the agent is reading that charge,
	// which is an ordinary concurrent operator or retry worker rather than a
	// poisoned observation.
	policy, err := chaos.Parse([]byte("version: 1\nrules:\n"+
		"  - id: partial-refund-by-another-writer\n    type: concurrent_mutation\n    operations: [getCharge]\n    times: 1\n"+
		"    actor:\n      operation: createRefund\n      arguments: {charge_id: CH-1002, amount_cents: 500, reason: concurrent operator}\n"), manifest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := policy.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "partial.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	world, err := s.SeedScenario(ctx, 42, manifest.Digest, "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRunWithChaos(ctx, world.ID, "company-incident", "scripted", "fixture-v1",
		"Investigate the duplicate charge and make the customer whole.", "", encoded, policy.Digest())
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{
		Store:    s,
		Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest},
		Manifest: manifest,
		Provider: agent.CompanyScriptedProvider{Scenario: "company-incident"},
	}
	if _, err = runner.Execute(ctx, run.ID, 25); err != nil {
		t.Fatalf("execute: %v", err)
	}

	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	injected, refused, agentRefunds := 0, 0, 0
	var requested []int64
	for _, event := range events {
		switch event.Type {
		case "chaos.injected":
			var payload struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Type == "concurrent_mutation" {
				injected++
			}
		case "tool.request":
			var payload struct {
				OperationID string         `json:"operation_id"`
				Arguments   map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.OperationID == "createRefund" {
				agentRefunds++
				if amount, ok := payload.Arguments["amount_cents"].(float64); ok {
					requested = append(requested, int64(amount))
				}
			}
		case "tool.response":
			var payload struct {
				OperationID string `json:"operation_id"`
				Status      int    `json:"status"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.OperationID == "createRefund" && payload.Status == 409 {
				refused++
			}
		}
	}
	if injected != 1 {
		t.Fatalf("concurrent mutations injected = %d, want 1; the fault is not reaching this trajectory", injected)
	}

	// The invariant that actually matters to the customer, and the one the
	// agent's own transcript cannot show. The charge must end fully refunded and
	// never over-refunded, whatever order the two writers landed in.
	var amount, refunded int64
	if err := s.DB.QueryRowContext(ctx,
		"SELECT amount_cents,refunded_cents FROM charges WHERE world_id=? AND id='CH-1002'",
		world.ID).Scan(&amount, &refunded); err != nil {
		t.Fatal(err)
	}
	if refunded != amount {
		t.Fatalf("CH-1002 refunded %d of %d after a concurrent partial refund; the customer is short %d (agent refund requests=%v, refusals=%d)",
			refunded, amount, amount-refunded, requested, refused)
	}

	// And the route, not just the total. Two requests in this exact order are
	// what remainder recovery looks like: the full amount computed from the
	// invoice, refused once, then only what the read-back showed outstanding. A
	// single request could not have reached a whole customer, and a second
	// request for the full amount could not have been accepted.
	if refused != 1 {
		t.Fatalf("refund refusals = %d, want 1; the 409 path is not being exercised", refused)
	}
	if len(requested) != 2 || requested[0] != amount || requested[1] != amount-500 {
		t.Fatalf("refund requests = %v, want [%d %d]: the full amount, refused, then the remainder",
			requested, amount, amount-500)
	}

	// The built-in judge is not the authority here, and this is the same
	// single-writer assumption ADR 0025 records for the bench suite: its
	// duplicate-charge checks count refund ROWS, and a concurrent writer makes
	// two rows correct. Those row counts are what catch the genuine double
	// refund elsewhere, so they stay as they are; what this test pins is that
	// they are the ONLY thing a second writer disturbs.
	report, err := Evaluate(ctx, s, world.ID, "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	failed := []string{}
	for _, check := range report.Checks {
		if !check.Passed {
			failed = append(failed, check.Name)
		}
	}
	expected := map[string]bool{"correct_charge_identified": true, "exactly_one_refund": true}
	for _, name := range failed {
		if !expected[name] {
			t.Fatalf("a concurrent partial refund disturbed check %q, which does not count refund rows; failed=%v", name, failed)
		}
	}
	if len(failed) != len(expected) {
		t.Fatalf("expected exactly the two row-counting checks to fail under a second writer, got %v", failed)
	}
}
