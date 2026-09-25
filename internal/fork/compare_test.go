package fork

import (
	"context"
	"testing"
)

func TestCompareReportsCounterfactualStateAndTrajectory(t *testing.T) {
	source, destination, parent, manifest := completedBilling(t)
	ctx := context.Background()
	selected := operationCheckpoint(t, source, parent.ID, manifest, "model.response", "createRefund")
	created, err := Create(ctx, source, destination, selected, manifest, Options{})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Compare(ctx, destination, parent.ID, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Parent.Status != "completed" || report.Child.Status != "paused" || report.ForkEventSeq != selected.EventSeq {
		t.Fatalf("comparison status=%+v", report)
	}
	if report.Parent.Evaluation == nil || !report.Parent.Evaluation.Passed || report.Child.Evaluation != nil {
		t.Fatalf("comparison evaluation=%+v", report)
	}
	if len(report.Parent.ToolTrajectory) == 0 || len(report.Child.ToolTrajectory) != 0 {
		t.Fatalf("tool trajectories=%+v %+v", report.Parent.ToolTrajectory, report.Child.ToolTrajectory)
	}
	foundRefundDifference := false
	for _, difference := range report.StateDifferences {
		if difference.Table == "refunds" && len(difference.ParentOnly) == 1 && len(difference.ChildOnly) == 0 {
			foundRefundDifference = true
		}
	}
	if !foundRefundDifference || report.Latency.Available || report.ModelUsage.Available {
		t.Fatalf("comparison differences=%+v", report)
	}
	if _, err := Compare(ctx, destination, created.Run.ID, parent.ID); err == nil {
		t.Fatal("reversed lineage accepted")
	}
}
