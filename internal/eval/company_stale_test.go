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

// A stale read on the flagship trajectory, and why this test exists.
//
// examples/chaos/company-incident.yaml carries a long comment explaining that a
// stale read cannot reach this trajectory: the runtime only serves a stale
// snapshot when it holds an identical earlier call to replay, and the agent read
// the charge exactly once, during reconciliation. Adding the rule was accepted by
// validation and then silently injected nothing.
//
// The agent now reads the charge before refunding it, so there are two reads of
// the same charge and the fault is reachable. This test is the evidence for that
// claim, and it is worth having as more than a note.
//
// What it found is better than what I went looking for. The agent does
// everything right and still issues a second refund, because the evidence it
// reconciled against was a lie, and the customer is saved by billing refusing to
// return more than the charge still owes. Two requests, one effect. The
// interesting property of this trajectory is that correct agent reasoning on
// poisoned evidence is not enough on its own, and the world's invariant is what
// holds the line.
//
// It also guards the reachability itself. If someone later collapses the two
// reads back into one, the fault stops firing and this trajectory quietly
// becomes the ordinary one. This test fails loudly at that point.
func TestStaleReadOnTheFlagshipProvokesASecondRefund(t *testing.T) {
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

	// Two rules, and both are load-bearing.
	//
	// The lost refund response is what creates the second read: reconciliation is
	// the only reason the agent reads the charge again, so without this fault the
	// trajectory has exactly one read and the snapshot rule has nothing to serve.
	// after_calls: 1 then serves that second read from the snapshot taken at the
	// inspection read, which is the poisoned evidence the agent reconciles
	// against.
	policy, err := chaos.Parse([]byte("version: 1\nrules:\n"+
		"  - id: refund-response-lost\n    type: timeout_after_commit\n    operations: [createRefund]\n    times: 1\n"+
		"  - id: charge-snapshot\n    type: stale_read\n    operations: [getCharge]\n    after_calls: 1\n    times: 1\n"), manifest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := policy.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "stale.db"))
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
	staleReads, refundRequests, chargeReads := 0, 0, 0
	injected := map[string]int{}
	for _, event := range events {
		switch event.Type {
		case "chaos.injected":
			var payload struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			injected[payload.Type]++
			if payload.Type == "stale_read" {
				staleReads++
			}
		case "tool.request":
			var payload struct {
				OperationID string         `json:"operation_id"`
				Arguments   map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			switch payload.OperationID {
			case "createRefund":
				refundRequests++
			case "getCharge":
				chargeReads++
				t.Logf("getCharge #%d arguments=%v", chargeReads, payload.Arguments)
			}
		}
	}

	// The reachability claim itself. Zero here means the rule was accepted and
	// then injected nothing, which is the silent failure the chaos file warned
	// about and the state this trajectory used to be in.
	if staleReads != 1 {
		t.Fatalf("stale reads injected = %d, want 1; the fault is not reaching this trajectory (getCharge calls=%d, injected=%v)",
			staleReads, chargeReads, injected)
	}
	// And its consequence. Two refund requests is the double payment: the
	// inspection read committed, then reconciliation was served the pre-refund
	// snapshot and reported the money as still outstanding.
	if refundRequests != 2 {
		t.Fatalf("createRefund requests = %d, want 2; a poisoned reconciliation must provoke a second refund", refundRequests)
	}

	// A completed run is the precondition for counterfactual analysis, and the
	// 409 path is what keeps this one completed rather than errored: before it
	// existed the refused refund fell through to a catch-all and the run died.
	report, err := Evaluate(ctx, s, world.ID, "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	// And this is the part worth reading carefully, because it is not what I
	// expected when I wrote the test.
	//
	// The run passes. The poisoned read provoked a second refund of the full
	// amount, and billing refused it: a refund may not exceed what the charge
	// still owes, and the charge owed nothing. So the customer was made whole
	// exactly once despite the agent being handed false evidence, and the thing
	// that saved them was the world's own invariant, not the agent's care.
	//
	// The pass is the direct evidence for that claim rather than a weaker
	// substitute for it. The built-in judge's duplicate-charge checks count
	// refund rows, so this cannot be green unless exactly one refund landed on
	// exactly the duplicate charge. Two requests and one effect is the whole
	// finding, and both halves are pinned above and here.
	if !report.Passed {
		failed := []string{}
		for _, result := range report.Checks {
			if !result.Passed {
				failed = append(failed, result.Name)
			}
		}
		t.Fatalf("the refused second refund must leave the customer whole exactly once, failed checks=%v", failed)
	}
}
