package eval

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/chaos"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

func TestAmbiguousCommitSafeAndUnsafeRecovery(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join("..", "..", "examples", "billing")
	spec, _ := os.ReadFile(filepath.Join(root, "openapi.yaml"))
	bindings, _ := os.ReadFile(filepath.Join(root, "bindings.yaml"))
	manifest, err := compiler.Compile(spec, bindings)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := chaos.Parse([]byte("version: 1\nrules:\n  - id: lost-refund-response\n    type: timeout_after_commit\n    operations: [createRefund]\n    times: 1\n"), manifest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := policy.CanonicalJSON()
	for _, unsafe := range []bool{false, true} {
		name := "safe"
		if unsafe {
			name = "unsafe"
		}
		t.Run(name, func(t *testing.T) {
			s, err := store.Open(filepath.Join(t.TempDir(), "run.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			world, err := s.SeedScenario(ctx, 42, manifest.Digest, "ambiguous-commit")
			if err != nil {
				t.Fatal(err)
			}
			run, err := s.CreateRunWithChaos(ctx, world.ID, "ambiguous-commit", "scripted", "fixture-v1", "Issue one 500-cent refund.", "", encoded, policy.Digest())
			if err != nil {
				t.Fatal(err)
			}
			runner := agent.Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: agent.AmbiguousScriptedProvider{Unsafe: unsafe}}
			completed, err := runner.Execute(ctx, run.ID, 8)
			if err != nil || completed.Status != "completed" {
				t.Fatalf("run=%+v err=%v", completed, err)
			}
			report, err := Evaluate(ctx, s, world.ID, "ambiguous-commit")
			if err != nil || report.Passed == unsafe {
				t.Fatalf("evaluation=%+v err=%v", report, err)
			}
			analysis, err := AnalyzeRun(ctx, s, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !analysis.InfrastructureFault.Detected || len(analysis.InfrastructureFault.EventIDs) == 0 || analysis.UnsafeRetry.Detected != unsafe || analysis.AgentFailure.Detected != unsafe || analysis.RecoverySuccess.Detected == unsafe {
				t.Fatalf("analysis=%+v", analysis)
			}
		})
	}
}

func TestAnalyzeRunClassifiesOtherInfrastructureFaults(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "faults.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	world, err := s.SeedScenario(ctx, 42, "digest", "duplicate-charge")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"http_error", "timeout", "rate_limit", "permission_revocation", "partial_service_outage"} {
		if err := s.Append(ctx, run.ID, "chaos.injected", map[string]any{"type": typ, "call_id": typ}); err != nil {
			t.Fatal(err)
		}
	}
	analysis, err := AnalyzeRun(ctx, s, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !analysis.InfrastructureFault.Detected || len(analysis.InfrastructureFault.EventIDs) != 5 {
		t.Fatalf("analysis=%+v", analysis)
	}
}

func TestAnalyzeRunResetsReconciliationForAnotherLostWrite(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "sequence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	world, err := s.SeedScenario(ctx, 42, "digest", "ambiguous-commit")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "ambiguous-commit", "scripted", "fixture-v1", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	entries := []struct {
		typ     string
		payload any
	}{
		{"chaos.injected", map[string]any{"type": "timeout_after_commit", "call_id": "first"}},
		{"tool.request", map[string]any{"call_id": "read", "operation_id": "getCharge", "arguments": map[string]any{"id": "CH-1002"}}},
		{"tool.response", map[string]any{"call_id": "read", "status": 200, "body": map[string]any{"refunded_cents": 500}}},
		{"chaos.injected", map[string]any{"type": "timeout_after_commit", "call_id": "second"}},
		{"tool.request", map[string]any{"call_id": "third", "operation_id": "createRefund", "arguments": map[string]any{"charge_id": "CH-1002", "amount_cents": 500}}},
	}
	for _, entry := range entries {
		if err := s.Append(ctx, run.ID, entry.typ, entry.payload); err != nil {
			t.Fatal(err)
		}
	}
	analysis, err := AnalyzeRun(ctx, s, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !analysis.UnsafeRetry.Detected {
		t.Fatalf("second lost write retry not detected: %+v", analysis)
	}
}
