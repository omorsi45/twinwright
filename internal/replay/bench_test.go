package replay

import (
	"context"
	"testing"

	"twinwright/internal/checkpoint"
)

// Benchmarks for the two verification paths an engineer waits on: proving a
// recorded run reproduces, and rebuilding a world at a checkpoint.
//
// Both re-execute the run against a fresh world, so the figures include the
// scripted provider and the simulated services. That is the honest unit: replay
// verification is not a database read, it is a re-execution plus a comparison,
// and reporting only the comparison would understate what the command costs.
//
// The recorded run is built once outside the timer. Every iteration verifies
// that same run, so the numbers are comparable between changes to the replay
// and checkpoint code.

// BenchmarkVerify measures replay verification of a completed billing run,
// including its fault retry and its pause.
func BenchmarkVerify(b *testing.B) {
	ctx := context.Background()
	source, run, manifest := completedRun(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		report, err := Verify(ctx, source, run.ID, manifest)
		if err != nil {
			b.Fatalf("verify: %v", err)
		}
		if !report.Verified {
			b.Fatalf("replay diverged: %s", report.Divergence)
		}
	}
}

// BenchmarkCheckpointList measures listing the reconstructable points of a run,
// which hashes every ledger prefix.
func BenchmarkCheckpointList(b *testing.B) {
	ctx := context.Background()
	source, run, manifest := completedRun(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		points, err := checkpoint.List(ctx, source, run.ID, manifest)
		if err != nil {
			b.Fatalf("list: %v", err)
		}
		if len(points) == 0 {
			b.Fatal("no checkpoints")
		}
	}
}

// BenchmarkCheckpointReconstruct measures rebuilding a world and run at the
// first tool call, which is what a fork or a counterfactual pays before it can
// diverge.
func BenchmarkCheckpointReconstruct(b *testing.B) {
	ctx := context.Background()
	source, run, manifest := completedRun(b)
	selected := firstCheckpoint(b, source, run.ID, manifest, "tool.response")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rebuilt, _, err := checkpoint.Reconstruct(ctx, source, run.ID, selected, manifest)
		if err != nil {
			b.Fatalf("reconstruct: %v", err)
		}
		b.StopTimer()
		rebuilt.Close()
		b.StartTimer()
	}
}
