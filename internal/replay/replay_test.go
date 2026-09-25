package replay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	if _, err := source.Seed(ctx, 42, manifest.Digest); err != nil {
		t.Fatal(err)
	}
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
	wrong := compiler.Manifest{Operations: manifest.Operations[:1]}
	encoded, err := json.Marshal(wrong.Operations)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	wrong.Digest = hex.EncodeToString(digest[:])
	if err := compiler.ValidateManifest(wrong); err != nil {
		t.Fatalf("alternate manifest invalid: %v", err)
	}
	if _, err := Verify(ctx, source, run.ID, wrong); err == nil || !strings.Contains(err.Error(), "manifest mismatch for run") {
		t.Fatalf("wrong valid manifest error=%v", err)
	}
	_, err = source.DB.ExecContext(ctx, "UPDATE runs SET status='paused' WHERE id=?", run.ID)
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
	payload, err := json.Marshal(map[string]string{"kind": "provider", "message": "failed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.DB.ExecContext(ctx, "UPDATE events SET payload=? WHERE run_id=? AND type='error'", string(payload), run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, source, run.ID, manifest); err == nil || !strings.Contains(err.Error(), "unsupported provider error") {
		t.Fatalf("failed model turn error=%v", err)
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
	if _, err := source.DB.ExecContext(ctx, "UPDATE events SET type='unexpected.event' WHERE run_id=? AND type='execution.paused'", run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, source, run.ID, manifest); err == nil || !strings.Contains(err.Error(), "unknown event type") {
		t.Fatalf("unknown event error=%v", err)
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

type decimalArgumentProvider struct{}

func (decimalArgumentProvider) Next(_ context.Context, _ string, history []agent.Message, _ []compiler.Operation) (agent.Message, error) {
	if len(history) == 0 {
		return agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{
			ID: "decimal-refund", OperationID: "createRefund",
			Arguments: map[string]any{"charge_id": "CH-1002", "amount_cents": json.Number("1000.0"), "reason": "duplicate"},
		}}}, nil
	}
	return agent.Message{Role: "assistant", Content: "Done."}, nil
}

func TestVerifyPreservesRecordedNumberRepresentation(t *testing.T) {
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
	source, err := store.Open(filepath.Join(t.TempDir(), "numeric.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.Seed(ctx, 42, manifest.Digest); err != nil {
		t.Fatal(err)
	}
	world, err := source.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := source.CreateRun(ctx, world.ID, "duplicate-charge", "decimal-test", "fixture-v1", "try refund", "")
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Store: source, Dispatch: &dispatch.Dispatcher{Store: source, Manifest: manifest}, Manifest: manifest, Provider: decimalArgumentProvider{}}
	completed, err := runner.Execute(ctx, run.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "completed" {
		t.Fatalf("status=%s", completed.Status)
	}
	report, err := Verify(ctx, source, run.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified {
		t.Fatalf("unchanged numeric run diverged: %+v", report)
	}
}
