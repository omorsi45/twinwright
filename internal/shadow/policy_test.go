package shadow

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/authz"
	"twinwright/internal/compiler"
)

// The scripted fixture proposes a refund. A support principal that may read
// customers, invoices and charges but was never granted refunds.create would be
// denied that call by the dispatcher, and the observed stream contains a
// successful refund because a human with other permissions made it.
//
// Reporting the refund as matched and saying nothing about authorization is the
// failure this screens for: the comparison would read as "the agent did what the
// human did", when what the agent proposed is a call its own policy refuses.
func TestSimulateScreensProposedActionsAgainstThePolicy(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	spec, err := os.ReadFile(filepath.Join(root, "billing", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := os.ReadFile(filepath.Join(root, "billing", "bindings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := compiler.Compile(spec, bindings)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := authz.Parse([]byte("version: 1\nprincipal: {id: support-agent-1}\npermissions: {allow: [customers.read, invoices.read, charges.read]}\nresources: {customer_ids: [C-104]}\n"), manifest)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Simulate(context.Background(), SimulateOptions{
		Manifest: manifest, Scenario: "duplicate-charge",
		Task:     "Customer C-104 says they were charged twice. Investigate the account and refund only the duplicate charge if appropriate.",
		Provider: agent.ScriptedProvider{}, AgentName: "scripted", Model: "fixture-v1", Steps: 20,
		WorkDir: t.TempDir(), Policy: &policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Policy == nil {
		t.Fatal("a configured policy produced no screen")
	}
	if result.Policy.Principal != "support-agent-1" || result.Policy.Screened != len(result.Proposed) {
		t.Fatalf("%+v over %d proposed actions", result.Policy, len(result.Proposed))
	}
	if len(result.Policy.Refusals) != 1 {
		t.Fatalf("want the refund refused and nothing else: %+v", result.Policy.Refusals)
	}
	refusal := result.Policy.Refusals[0]
	if refusal.OperationID != "createRefund" || refusal.Reason != authz.ReasonNotGranted || refusal.Permission != "refunds.create" {
		t.Fatalf("%+v", refusal)
	}
	if refusal.Call != 4 {
		t.Fatalf("the refusal must carry the run-local call number the policy decided at: %+v", refusal)
	}

	observed, err := LoadObservations(root, "shadow/sample-observations.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	cmp, err := CompareWith(result.Proposed, observed, CompareOptions{Policy: result.Policy})
	if err != nil {
		t.Fatal(err)
	}
	if len(cmp.Policy.Refused) != 1 || !cmp.Policy.Refused[0].AlsoObserved {
		t.Fatalf("a refusal must survive an identical observed action: %+v", cmp.Policy)
	}
	if len(cmp.Matched) < 2 {
		t.Fatalf("the screen must not change what matched: %+v", cmp)
	}
}
