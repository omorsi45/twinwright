package shadow

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/compiler"
)

func TestSimulateAndCompare(t *testing.T) {
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
	proposed, runID, err := Simulate(context.Background(), SimulateOptions{
		Manifest: manifest, Scenario: "duplicate-charge",
		Task:     "Customer C-104 says they were charged twice. Investigate the account and refund only the duplicate charge if appropriate.",
		Provider: agent.ScriptedProvider{}, AgentName: "scripted", Model: "fixture-v1", Steps: 20, WorkDir: t.TempDir(),
	})
	if err != nil || runID == "" || len(proposed) < 3 {
		t.Fatalf("proposed=%v run=%s err=%v", proposed, runID, err)
	}
	obs, err := LoadObservations(root, "shadow/sample-observations.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	cmp, err := Compare(proposed, obs)
	if err != nil {
		t.Fatal(err)
	}
	if cmp.ProposedCount < 3 || cmp.ObservedCount != 3 || len(cmp.Matched) < 2 {
		t.Fatalf("%+v", cmp)
	}
}

func TestCompareNoWinnerLanguage(t *testing.T) {
	cmp, err := Compare(
		[]ProposedAction{{OperationID: "a", Arguments: map[string]any{"x": 1}}},
		[]Observation{{Kind: "human_action", OperationID: "b", Arguments: map[string]any{"y": 2}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmp.Matched) != 0 || len(cmp.OnlyProposed) != 1 || len(cmp.OnlyObserved) != 1 {
		t.Fatalf("%+v", cmp)
	}
}
