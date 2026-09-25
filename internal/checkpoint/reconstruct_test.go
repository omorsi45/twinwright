package checkpoint

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/behavior"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

func TestReconstructVerifiedToolBoundary(t *testing.T) {
	source, runID, manifest := checkpointFixture(t)
	ctx := context.Background()
	points, err := List(ctx, source, runID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	before, err := source.Events(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, run, err := Reconstruct(ctx, source, runID, points[1], manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	if run.ID != runID || run.Step != 1 || !strings.Contains(run.Transcript, "C-104") {
		t.Fatalf("reconstructed run=%+v", run)
	}
	generated, err := rebuilt.Events(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(generated) != points[1].EventSeq {
		t.Fatalf("reconstructed event count=%d, want %d", len(generated), points[1].EventSeq)
	}
	after, err := source.Events(ctx, runID)
	if err != nil || len(after) != len(before) {
		t.Fatalf("source changed: before=%d after=%d err=%v", len(before), len(after), err)
	}
}

func TestReconstructRestoresRefundStateBeforeCompletion(t *testing.T) {
	source, runID, manifest := checkpointFixture(t)
	ctx := context.Background()
	runner := agent.Runner{Store: source, Dispatch: &dispatch.Dispatcher{Store: source, Manifest: manifest}, Manifest: manifest, Provider: agent.ScriptedProvider{}}
	completed, err := runner.Execute(ctx, runID, 20)
	if err != nil || completed.Status != "completed" {
		t.Fatalf("completed=%+v, err=%v", completed, err)
	}
	points, err := List(ctx, source, runID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	events, err := source.Events(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var refundPoint Checkpoint
	for _, event := range events {
		if event.Type != "tool.response" {
			continue
		}
		var response struct {
			OperationID string `json:"operation_id"`
		}
		if err := json.Unmarshal(event.Payload, &response); err != nil {
			t.Fatal(err)
		}
		if response.OperationID == "createRefund" {
			refundPoint, err = Select(points, event.Seq)
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if refundPoint.ID == "" {
		t.Fatal("refund checkpoint missing")
	}
	rebuilt, run, err := Reconstruct(ctx, source, runID, refundPoint, manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	sourceState, err := source.Snapshot(ctx, completed.WorldID)
	if err != nil {
		t.Fatal(err)
	}
	rebuiltState, err := rebuilt.Snapshot(ctx, run.WorldID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rebuiltState, "refund") || rebuiltState != sourceState {
		t.Fatalf("refund state differs: source=%s rebuilt=%s", sourceState, rebuiltState)
	}
}

func TestReconstructRejectsTamperedSavedToolResult(t *testing.T) {
	source, runID, manifest := checkpointFixture(t)
	ctx := context.Background()
	points, err := List(ctx, source, runID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.DB.ExecContext(ctx, "UPDATE tool_results SET body='{}' WHERE run_id=? AND call_id='call-1'", runID); err != nil {
		t.Fatal(err)
	}
	if rebuilt, _, err := Reconstruct(ctx, source, runID, points[1], manifest); err == nil {
		rebuilt.Close()
		t.Fatal("tampered saved result accepted")
	}
}

func TestReconstructCompanyIncidentAcrossServices(t *testing.T) {
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
	source, err := store.Open(filepath.Join(t.TempDir(), "company.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	world, err := source.SeedScenario(ctx, 42, manifest.Digest, "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	run, err := source.CreateRun(ctx, world.ID, "company-incident", "scripted", "fixture-v1", "task", "messagePostMessage")
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Store: source, Dispatch: &dispatch.Dispatcher{Store: source, Manifest: manifest}, Manifest: manifest, Provider: agent.CompanyScriptedProvider{Scenario: "company-incident"}}
	completed, err := runner.Execute(ctx, run.ID, 40)
	if err != nil || completed.Status != "completed" {
		t.Fatalf("run=%+v, err=%v", completed, err)
	}
	points, err := List(ctx, source, run.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, restored, err := Reconstruct(ctx, source, run.ID, points[len(points)-1], manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer rebuilt.Close()
	for _, table := range []string{"refunds", "crm_notes", "ticket_issues", "ticket_comments", "message_messages"} {
		var originalCount, restoredCount int
		query := "SELECT count(*) FROM " + table + " WHERE world_id=?"
		if err := source.DB.QueryRowContext(ctx, query, world.ID).Scan(&originalCount); err != nil {
			t.Fatal(err)
		}
		if err := rebuilt.DB.QueryRowContext(ctx, query, restored.WorldID).Scan(&restoredCount); err != nil {
			t.Fatal(err)
		}
		if originalCount == 0 || restoredCount != originalCount {
			t.Fatalf("%s row count: source=%d restored=%d", table, originalCount, restoredCount)
		}
	}
}

func TestReconstructRejectsTamperedToolResultBeforeCreatingChild(t *testing.T) {
	source, runID, manifest := checkpointFixture(t)
	ctx := context.Background()
	points, err := List(ctx, source, runID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.DB.ExecContext(ctx, "UPDATE events SET payload=? WHERE run_id=? AND seq=5", `{"call_id":"call-1","operation_id":"getCustomer","status":200,"body":{"id":"C-104","name":"tampered"}}`, runID); err != nil {
		t.Fatal(err)
	}
	if rebuilt, _, err := Reconstruct(ctx, source, runID, points[1], manifest); err == nil {
		rebuilt.Close()
		t.Fatal("tampered checkpoint accepted")
	}
}
