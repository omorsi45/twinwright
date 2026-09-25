package fork

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/behavior"
	"twinwright/internal/checkpoint"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

func completedBilling(t *testing.T) (*store.Store, *store.Store, store.Run, compiler.Manifest) {
	t.Helper()
	ctx := context.Background()
	root := filepath.Join("..", "..", "examples", "billing")
	spec, err := os.ReadFile(filepath.Join(root, "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := os.ReadFile(filepath.Join(root, "bindings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := compiler.Compile(spec, bindings)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "world.db")
	destination, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { destination.Close() })
	world, err := destination.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := destination.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Store: destination, Dispatch: &dispatch.Dispatcher{Store: destination, Manifest: manifest}, Manifest: manifest, Provider: agent.ScriptedProvider{}}
	completed, err := runner.Execute(ctx, run.ID, 20)
	if err != nil || completed.Status != "completed" {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
	source, err := store.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	return source, destination, completed, manifest
}

func operationCheckpoint(t *testing.T, source *store.Store, runID string, manifest compiler.Manifest, eventType, operation string) checkpoint.Checkpoint {
	t.Helper()
	ctx := context.Background()
	points, err := checkpoint.List(ctx, source, runID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	events, err := source.Events(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != eventType {
			continue
		}
		var detail struct {
			OperationID string `json:"operation_id"`
			ToolCalls   []struct {
				OperationID string `json:"operation_id"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(event.Payload, &detail); err != nil {
			t.Fatal(err)
		}
		match := detail.OperationID == operation
		for _, call := range detail.ToolCalls {
			match = match || call.OperationID == operation
		}
		if match {
			point, err := checkpoint.Select(points, event.Seq)
			if err != nil {
				t.Fatal(err)
			}
			return point
		}
	}
	t.Fatalf("checkpoint for %s %s missing", eventType, operation)
	return checkpoint.Checkpoint{}
}

func TestCreateForkPreservesParentAndPriorResults(t *testing.T) {
	source, destination, parent, manifest := completedBilling(t)
	ctx := context.Background()
	selected := operationCheckpoint(t, source, parent.ID, manifest, "tool.response", "createRefund")
	beforeEvents, err := source.Events(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeState, err := source.Snapshot(ctx, parent.WorldID)
	if err != nil {
		t.Fatal(err)
	}
	created, err := Create(ctx, source, destination, selected, manifest, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if created.Run.ID == parent.ID || created.Run.WorldID == parent.WorldID || created.Run.Status != "paused" {
		t.Fatalf("fork is not isolated: %+v", created)
	}
	lineage, err := destination.Lineage(ctx, created.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lineage.ParentRunID != parent.ID || lineage.ForkEventSeq != selected.EventSeq || lineage.CheckpointID != selected.ID {
		t.Fatalf("lineage=%+v", lineage)
	}
	childState, err := destination.Snapshot(ctx, created.Run.WorldID)
	if err != nil {
		t.Fatal(err)
	}
	if childState != beforeState {
		t.Fatalf("forked state differs: parent=%s child=%s", beforeState, childState)
	}
	childEvents, err := destination.Events(ctx, created.Run.ID)
	if err != nil || len(childEvents) != 1 || childEvents[0].Type != "execution.forked" {
		t.Fatalf("child events=%+v err=%v", childEvents, err)
	}
	d := dispatch.Dispatcher{Store: destination, Manifest: manifest}
	if _, err := d.Invoke(ctx, created.Run.ID, "fixture-refund", "createRefund", map[string]any{"charge_id": "CH-1002", "amount_cents": refundAmount(t, destination, created.Run.WorldID), "reason": "duplicate charge"}); err != nil {
		t.Fatal(err)
	}
	afterChildEvents, err := destination.Events(ctx, created.Run.ID)
	if err != nil || len(afterChildEvents) != len(childEvents) {
		t.Fatalf("inherited call was not idempotent: events=%+v err=%v", afterChildEvents, err)
	}
	afterEvents, err := source.Events(ctx, parent.ID)
	if err != nil || len(afterEvents) != len(beforeEvents) {
		t.Fatalf("parent events changed: %d to %d, err=%v", len(beforeEvents), len(afterEvents), err)
	}
	afterState, err := source.Snapshot(ctx, parent.WorldID)
	if err != nil || afterState != beforeState {
		t.Fatalf("parent state changed: %v", err)
	}
}

func TestCreateRejectsTamperedModelRequestAfterRelisting(t *testing.T) {
	ctx := context.Background()
	source, destination, parent, manifest := completedBilling(t)
	events, err := source.Events(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	var request store.Event
	for _, event := range events {
		if event.Type == "model.request" {
			request = event
			break
		}
	}
	if request.Seq == 0 {
		t.Fatal("model request missing")
	}
	var payload map[string]any
	if err := json.Unmarshal(request.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload["task"] = "tampered task"
	changed, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := destination.DB.ExecContext(ctx, "UPDATE events SET payload=? WHERE run_id=? AND seq=?", string(changed), parent.ID, request.Seq); err != nil {
		t.Fatal(err)
	}
	points, err := checkpoint.List(ctx, source, parent.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := checkpoint.Select(points, request.Seq+1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, source, destination, selected, manifest, Options{}); err == nil {
		t.Fatal("tampered model request accepted")
	}
	var children int
	if err := destination.DB.QueryRowContext(ctx, "SELECT count(*) FROM fork_lineage").Scan(&children); err != nil {
		t.Fatal(err)
	}
	if children != 0 {
		t.Fatalf("created %d child after tampered request", children)
	}
}

func refundAmount(t *testing.T, s *store.Store, worldID string) int64 {
	t.Helper()
	var amount int64
	if err := s.DB.QueryRow("SELECT amount_cents FROM charges WHERE world_id=? AND id='CH-1002'", worldID).Scan(&amount); err != nil {
		t.Fatal(err)
	}
	return amount
}

func TestCreateForkCanCompletePendingToolCall(t *testing.T) {
	source, destination, parent, manifest := completedBilling(t)
	ctx := context.Background()
	selected := operationCheckpoint(t, source, parent.ID, manifest, "model.response", "createRefund")
	created, err := Create(ctx, source, destination, selected, manifest, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var refunds int
	if err := destination.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", created.Run.WorldID).Scan(&refunds); err != nil {
		t.Fatal(err)
	}
	if refunds != 0 {
		t.Fatalf("pending refund already applied: %d", refunds)
	}
	runner := agent.Runner{Store: destination, Dispatch: &dispatch.Dispatcher{Store: destination, Manifest: manifest}, Manifest: manifest, Provider: agent.ScriptedProvider{}}
	completed, err := runner.Execute(ctx, created.Run.ID, 5)
	if err != nil || completed.Status != "completed" {
		t.Fatalf("fork continuation=%+v err=%v", completed, err)
	}
	if err := destination.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", created.Run.WorldID).Scan(&refunds); err != nil {
		t.Fatal(err)
	}
	if refunds != 1 {
		t.Fatalf("fork refund count=%d", refunds)
	}
}

func TestCreateForkAppliesNewFaultOnlyToChild(t *testing.T) {
	source, destination, parent, manifest := completedBilling(t)
	ctx := context.Background()
	selected := operationCheckpoint(t, source, parent.ID, manifest, "model.response", "createRefund")
	fault := "createRefund"
	created, err := Create(ctx, source, destination, selected, manifest, Options{Provider: "scripted", Model: "fixture-v2", FaultOperation: &fault})
	if err != nil {
		t.Fatal(err)
	}
	if created.Run.FaultOperation != fault || created.Run.Model != "fixture-v2" {
		t.Fatalf("fork options=%+v", created.Run)
	}
	d := dispatch.Dispatcher{Store: destination, Manifest: manifest}
	result, err := d.Invoke(ctx, created.Run.ID, "fixture-refund", "createRefund", map[string]any{"charge_id": "CH-1002", "amount_cents": refundAmount(t, destination, created.Run.WorldID), "reason": "duplicate charge"})
	if err != nil || result.Status != 503 {
		t.Fatalf("new fault response=%+v err=%v", result, err)
	}
	var parentRefunds, childRefunds int
	if err := destination.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", parent.WorldID).Scan(&parentRefunds); err != nil {
		t.Fatal(err)
	}
	if err := destination.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", created.Run.WorldID).Scan(&childRefunds); err != nil {
		t.Fatal(err)
	}
	if parentRefunds != 1 || childRefunds != 0 {
		t.Fatalf("fault leaked across worlds: parent=%d child=%d", parentRefunds, childRefunds)
	}
}

func TestCreateForkRollsBackOnLineageFailure(t *testing.T) {
	source, destination, parent, manifest := completedBilling(t)
	ctx := context.Background()
	selected := operationCheckpoint(t, source, parent.ID, manifest, "tool.response", "createRefund")
	var beforeWorlds, beforeRuns, beforePoints int
	for i, query := range []string{"SELECT count(*) FROM worlds", "SELECT count(*) FROM runs", "SELECT count(*) FROM checkpoints"} {
		var count int
		if err := destination.DB.QueryRowContext(ctx, query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		switch i {
		case 0:
			beforeWorlds = count
		case 1:
			beforeRuns = count
		case 2:
			beforePoints = count
		}
	}
	if _, err := destination.DB.ExecContext(ctx, `CREATE TRIGGER reject_fork BEFORE INSERT ON fork_lineage BEGIN SELECT RAISE(ABORT, 'lineage rejected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(ctx, source, destination, selected, manifest, Options{}); err == nil {
		t.Fatal("lineage failure accepted")
	}
	for i, query := range []string{"SELECT count(*) FROM worlds", "SELECT count(*) FROM runs", "SELECT count(*) FROM checkpoints"} {
		var count int
		if err := destination.DB.QueryRowContext(ctx, query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		want := []int{beforeWorlds, beforeRuns, beforePoints}[i]
		if count != want {
			t.Fatalf("partial fork remained after failure: %s count=%d want=%d", query, count, want)
		}
	}
}

func TestCreateForkCopiesEveryCompanyServiceTable(t *testing.T) {
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
	dbPath := filepath.Join(t.TempDir(), "company.db")
	destination, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	world, err := destination.SeedScenario(ctx, 42, manifest.Digest, "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	run, err := destination.CreateRun(ctx, world.ID, "company-incident", "scripted", "fixture-v1", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Store: destination, Dispatch: &dispatch.Dispatcher{Store: destination, Manifest: manifest}, Manifest: manifest, Provider: agent.CompanyScriptedProvider{Scenario: "company-incident"}}
	completed, err := runner.Execute(ctx, run.ID, 40)
	if err != nil || completed.Status != "completed" {
		t.Fatalf("company run=%+v err=%v", completed, err)
	}
	source, err := store.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	points, err := checkpoint.List(ctx, source, run.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	created, err := Create(ctx, source, destination, points[len(points)-1], manifest, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"customers", "invoices", "charges", "refunds", "subscriptions", "crm_accounts", "crm_contacts", "crm_notes", "ticket_projects", "ticket_issues", "ticket_comments", "message_workspaces", "message_channels", "message_members", "message_messages"} {
		query := "SELECT count(*) FROM " + table + " WHERE world_id=?"
		var originalCount, childCount int
		if err := destination.DB.QueryRowContext(ctx, query, world.ID).Scan(&originalCount); err != nil {
			t.Fatal(err)
		}
		if err := destination.DB.QueryRowContext(ctx, query, created.Run.WorldID).Scan(&childCount); err != nil {
			t.Fatal(err)
		}
		if originalCount == 0 || childCount != originalCount {
			t.Fatalf("%s rows: source=%d child=%d", table, originalCount, childCount)
		}
	}
}

func TestCreateForkRejectsUnknownProviderBeforeWriting(t *testing.T) {
	source, destination, parent, manifest := completedBilling(t)
	ctx := context.Background()
	selected := operationCheckpoint(t, source, parent.ID, manifest, "tool.response", "createRefund")
	if _, err := Create(ctx, source, destination, selected, manifest, Options{Provider: "does-not-exist"}); err == nil {
		t.Fatal("unknown provider accepted")
	}
	var count int
	if err := destination.DB.QueryRowContext(ctx, "SELECT count(*) FROM fork_lineage").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid fork wrote %d lineage rows", count)
	}
}
