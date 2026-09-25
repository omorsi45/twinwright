package replay

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

func completedRun(t *testing.T) (*store.Store, store.Run, compiler.Manifest) {
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
	source, err := store.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	world, err := source.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := source.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "Customer C-104 was charged twice. Refund only the duplicate.", "listCharges")
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Store: source, Dispatch: &dispatch.Dispatcher{Store: source, Manifest: manifest}, Manifest: manifest, Provider: agent.ScriptedProvider{}}
	paused, err := runner.Execute(ctx, run.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != "paused" {
		t.Fatalf("expected pause, got %s", paused.Status)
	}
	done, err := runner.Execute(ctx, run.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "completed" {
		t.Fatalf("expected completion, got %s", done.Status)
	}
	return source, done, manifest
}

func TestVerifyCompletedRunWith503AndPause(t *testing.T) {
	ctx := context.Background()
	source, run, manifest := completedRun(t)
	before, err := source.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Verify(ctx, source, run.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified || report.ModelTurns == 0 || report.ToolCalls == 0 || report.EventsCompared == 0 {
		t.Fatalf("replay report=%+v", report)
	}
	after, err := source.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("source ledger changed: %d to %d", len(before), len(after))
	}
}

func TestVerifyDetectsTamperedToolResponse(t *testing.T) {
	ctx := context.Background()
	source, run, manifest := completedRun(t)
	_, err := source.DB.ExecContext(ctx, "UPDATE events SET payload=? WHERE run_id=? AND type='tool.response' AND seq=(SELECT MIN(seq) FROM events WHERE run_id=? AND type='tool.response')", `{"call_id":"fixture-1","operation_id":"getCustomer","status":200,"body":{"id":"C-104","name":"tampered"}}`, run.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Verify(ctx, source, run.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified || report.Divergence == "" {
		t.Fatalf("tampered ledger accepted: %+v", report)
	}
}

func TestVerifyDetectsTamperedBillingState(t *testing.T) {
	ctx := context.Background()
	source, run, manifest := completedRun(t)
	_, err := source.DB.ExecContext(ctx, "UPDATE refunds SET reason='tampered' WHERE world_id=?", run.WorldID)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Verify(ctx, source, run.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified || report.Divergence != "refunds state differs" {
		t.Fatalf("tampered state accepted: %+v", report)
	}
}

func TestVerifyRejectsIncompleteRunAndWrongManifest(t *testing.T) {
	ctx := context.Background()
	source, run, manifest := completedRun(t)
	wrong := manifest
	wrong.Digest = "incorrect"
	if _, err := Verify(ctx, source, run.ID, wrong); err == nil {
		t.Fatal("wrong manifest accepted")
	}
	_, err := source.DB.ExecContext(ctx, "UPDATE runs SET status='paused' WHERE id=?", run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, source, run.ID, manifest); err == nil {
		t.Fatal("paused run accepted")
	}
}

func TestVerifyRejectsFailedModelTurnHistory(t *testing.T) {
	ctx := context.Background()
	source, run, manifest := completedRun(t)
	if err := source.Append(ctx, run.ID, "error", map[string]any{"kind": "provider", "message": "failed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, source, run.ID, manifest); err == nil {
		t.Fatal("failed model turn accepted")
	}
}

func TestJSONComparisonPreservesLargeIntegerDifferences(t *testing.T) {
	if sameJSON([]byte(`{"amount":9007199254740992}`), []byte(`{"amount":9007199254740993}`)) {
		t.Fatal("distinct integer payloads compare equal")
	}
}

func TestVerifyRejectsMissingCompletionEvent(t *testing.T) {
	ctx := context.Background()
	source, run, manifest := completedRun(t)
	if _, err := source.DB.ExecContext(ctx, "DELETE FROM events WHERE run_id=? AND type='execution.completed'", run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, source, run.ID, manifest); err == nil {
		t.Fatal("missing completion event accepted")
	}
}

func TestVerifyRejectsUnknownEventType(t *testing.T) {
	ctx := context.Background()
	source, run, manifest := completedRun(t)
	if err := source.Append(ctx, run.ID, "unexpected.event", map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, source, run.ID, manifest); err == nil {
		t.Fatal("unknown ledger event accepted")
	}
}

func TestVerifyRejectsStartMetadataMismatch(t *testing.T) {
	ctx := context.Background()
	source, run, manifest := completedRun(t)
	if _, err := source.DB.ExecContext(ctx, "UPDATE events SET payload=? WHERE run_id=? AND seq=1", `{"scenario":"duplicate-charge","provider":"other","model":"fixture-v1","world_id":"ignored"}`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, source, run.ID, manifest); err == nil {
		t.Fatal("start metadata mismatch accepted")
	}
}
