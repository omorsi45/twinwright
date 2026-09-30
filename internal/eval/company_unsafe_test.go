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

// The flagship needs a run that actually fails, and it has not had one.
//
// §9 asks the demo to end in a counterfactual that explains why the incident
// went wrong, but internal/counterfactual/analyze.go refuses a parent that
// passed: "counterfactual analysis explains failures". The safe company fixture
// passes, by design, so it cannot be its own subject. Borrowing the billing
// world's unsafe run would explain a different scenario in a different world,
// which is a worse story than the one the flagship is for.
//
// So the company world gets the same unsafe variant the billing world already
// has, selected the same way, by model name fixture-unsafe-v1. The difference
// between the two fixtures is one decision, and it is the decision the whole
// project is about: what to do when a write's response is lost.
//
// The fault here is timeout rather than timeout_after_commit, so the refund did
// NOT commit. The safe fixture reads the charge back, sees nothing returned and
// reissues, ending whole. The unsafe one assumes its write landed and moves on
// to the paperwork, so the customer is never refunded at all while the CRM note
// says they were. That is a failure the counterfactual can explain by naming a
// single observation and a single different branch, which is exactly what the
// demo's last step needs.
func TestUnsafeCompanyFixtureLeavesTheCustomerUnrefunded(t *testing.T) {
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
	policy, err := chaos.Parse([]byte("version: 1\nrules:\n"+
		"  - id: refund-never-committed\n    type: timeout\n    operations: [createRefund]\n    times: 1\n"), manifest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := policy.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "unsafe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	world, err := s.SeedScenario(ctx, 42, manifest.Digest, "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRunWithChaos(ctx, world.ID, "company-incident", "scripted", "fixture-unsafe-v1",
		"Investigate the duplicate charge and make the customer whole.", "", encoded, policy.Digest())
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{
		Store:    s,
		Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest},
		Manifest: manifest,
		Provider: agent.CompanyScriptedProvider{Scenario: "company-incident", Unsafe: true},
	}
	if _, err = runner.Execute(ctx, run.ID, 25); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// The unsafe fixture must not reconcile. One refund attempt, no read-back of
	// the charge after it. Without this the test could pass on a fixture that
	// recovered and merely happened to fail some unrelated check.
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, readsAfterAttempt := 0, 0
	for _, event := range events {
		if event.Type != "tool.request" {
			continue
		}
		var payload struct {
			OperationID string `json:"operation_id"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		switch payload.OperationID {
		case "createRefund":
			attempts++
		case "getCharge":
			if attempts > 0 {
				readsAfterAttempt++
			}
		}
	}
	if attempts != 1 {
		t.Fatalf("refund attempts = %d, want 1: the unsafe fixture must try once and move on", attempts)
	}
	if readsAfterAttempt != 0 {
		t.Fatalf("the unsafe fixture reconciled with %d read-backs; then it is not the unsafe branch", readsAfterAttempt)
	}

	// The outcome the counterfactual has to explain: money never returned.
	var refunded int64
	if err := s.DB.QueryRowContext(ctx,
		"SELECT refunded_cents FROM charges WHERE world_id=? AND id='CH-1002'", world.ID).Scan(&refunded); err != nil {
		t.Fatal(err)
	}
	if refunded != 0 {
		t.Fatalf("CH-1002 refunded %d; a timeout must leave the write uncommitted", refunded)
	}

	// And the judge has to see it, since a counterfactual parent that passes is
	// refused outright. exactly_one_refund is the check that carries it: the CRM
	// note claims a refund the ledger does not contain.
	report, err := Evaluate(ctx, s, world.ID, "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed {
		t.Fatal("the unsafe run passed; it cannot serve as a counterfactual parent")
	}
	failed := map[string]bool{}
	for _, check := range report.Checks {
		if !check.Passed {
			failed[check.Name] = true
		}
	}
	if !failed["exactly_one_refund"] {
		names := []string{}
		for name := range failed {
			names = append(names, name)
		}
		t.Fatalf("expected exactly_one_refund to fail on an unrefunded customer, failed=%v", names)
	}
}
