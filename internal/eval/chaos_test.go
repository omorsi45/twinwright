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
