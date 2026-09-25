package replay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/chaos"
	"twinwright/internal/checkpoint"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/fork"
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

func completedChaosRun(t *testing.T) (*store.Store, store.Run, compiler.Manifest) {
	t.Helper()
	ctx := context.Background()
	source, _, manifest := completedRun(t)
	world, err := source.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := chaos.Parse([]byte("version: 1\nrules:\n  - id: temporary-billing-failure\n    type: http_error\n    operations: [listCharges]\n    status: 503\n    times: 1\n"), manifest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := policy.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	run, err := source.CreateRunWithChaos(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "task", "", encoded, policy.Digest())
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Store: source, Dispatch: &dispatch.Dispatcher{Store: source, Manifest: manifest}, Manifest: manifest, Provider: agent.ScriptedProvider{}}
	done, err := runner.Execute(ctx, run.ID, 20)
	if err != nil || done.Status != "completed" {
		t.Fatalf("chaos run=%+v err=%v", done, err)
	}
	return source, done, manifest
}

func TestVerifyChaosRunAndFork(t *testing.T) {
	ctx := context.Background()
	source, parent, manifest := completedChaosRun(t)
	rootReport, err := Verify(ctx, source, parent.ID, manifest)
	if err != nil || !rootReport.Verified {
		t.Fatalf("root replay=%+v err=%v", rootReport, err)
	}
	points, err := checkpoint.List(ctx, source, parent.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, eventType := range []string{"model.response", "tool.response"} {
		t.Run(eventType, func(t *testing.T) {
			var selected checkpoint.Checkpoint
			for _, point := range points {
				if point.EventType == eventType {
					selected = point
					break
				}
			}
			if selected.ID == "" {
				t.Fatalf("no %s checkpoint", eventType)
			}
			created, err := fork.Create(ctx, source, source, selected, manifest, fork.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := source.ChaosPolicy(ctx, created.Run.ID); err != nil {
				t.Fatalf("child lost chaos policy: %v", err)
			}
			runner := agent.Runner{Store: source, Dispatch: &dispatch.Dispatcher{Store: source, Manifest: manifest}, Manifest: manifest, Provider: agent.ScriptedProvider{}}
			child, err := runner.Execute(ctx, created.Run.ID, 20)
			if err != nil || child.Status != "completed" {
				t.Fatalf("child=%+v err=%v", child, err)
			}
			childReport, err := Verify(ctx, source, child.ID, manifest)
			if err != nil || !childReport.Verified {
				t.Fatalf("child replay=%+v err=%v", childReport, err)
			}
		})
	}
}

func TestVerifyChaosRejectsTamperedPolicyAndEvent(t *testing.T) {
	for _, update := range []string{
		"UPDATE run_chaos SET policy_json='{}' WHERE run_id=?",
		"UPDATE events SET payload='{}' WHERE run_id=? AND type='chaos.injected'",
	} {
		t.Run(update, func(t *testing.T) {
			source, run, manifest := completedChaosRun(t)
			if _, err := source.DB.ExecContext(context.Background(), update, run.ID); err != nil {
				t.Fatal(err)
			}
			if report, err := Verify(context.Background(), source, run.ID, manifest); err == nil && report.Verified {
				t.Fatal("tampered chaos history accepted")
			}
		})
	}
}

func TestVerifyChaosRejectsTamperedCounter(t *testing.T) {
	ctx := context.Background()
	source, run, manifest := completedChaosRun(t)
	if _, err := source.DB.ExecContext(ctx, "UPDATE chaos_rule_state SET matching_calls=99 WHERE run_id=?", run.ID); err != nil {
		t.Fatal(err)
	}
	report, err := Verify(ctx, source, run.ID, manifest)
	if err == nil && report.Verified {
		t.Fatal("tampered chaos counter accepted")
	}
}

func completedFork(t *testing.T) (*store.Store, store.Run, store.Run, compiler.Manifest) {
	t.Helper()
	ctx := context.Background()
	source, parent, manifest := completedRun(t)
	points, err := checkpoint.List(ctx, source, parent.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	var selected checkpoint.Checkpoint
	for _, point := range points {
		if point.EventType == "model.response" {
			selected = point
			break
		}
	}
	if selected.ID == "" {
		t.Fatal("model response checkpoint missing")
	}
	fault := ""
	created, err := fork.Create(ctx, source, source, selected, manifest, fork.Options{Model: "alternative-fixture", FaultOperation: &fault})
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Store: source, Dispatch: &dispatch.Dispatcher{Store: source, Manifest: manifest}, Manifest: manifest, Provider: agent.ScriptedProvider{}}
	child, err := runner.Execute(ctx, created.Run.ID, 20)
	if err != nil || child.Status != "completed" {
		t.Fatalf("child=%+v err=%v", child, err)
	}
	return source, parent, child, manifest
}

func TestVerifyCompletedForkWithChangedModelAndFault(t *testing.T) {
	source, _, child, manifest := completedFork(t)
	report, err := Verify(context.Background(), source, child.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified || report.ToolCalls == 0 || report.ModelTurns == 0 {
		t.Fatalf("fork replay=%+v", report)
	}
}

func TestVerifyForkFromCompletionWithoutNewModelTurn(t *testing.T) {
	ctx := context.Background()
	source, parent, manifest := completedRun(t)
	points, err := checkpoint.List(ctx, source, parent.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	selected := points[len(points)-1]
	if selected.EventType != "execution.completed" {
		t.Fatalf("last checkpoint=%+v", selected)
	}
	created, err := fork.Create(ctx, source, source, selected, manifest, fork.Options{})
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Store: source, Dispatch: &dispatch.Dispatcher{Store: source, Manifest: manifest}, Manifest: manifest, Provider: agent.ScriptedProvider{}}
	child, err := runner.Execute(ctx, created.Run.ID, 1)
	if err != nil || child.Status != "completed" {
		t.Fatalf("child=%+v err=%v", child, err)
	}
	report, err := Verify(ctx, source, child.ID, manifest)
	if err != nil || !report.Verified || report.ModelTurns != 0 {
		t.Fatalf("fork replay=%+v err=%v", report, err)
	}
}

func TestVerifyForkCopiesOnlyPrefixToolResults(t *testing.T) {
	ctx := context.Background()
	source, parent, manifest := completedRun(t)
	points, err := checkpoint.List(ctx, source, parent.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	var selected checkpoint.Checkpoint
	for _, point := range points {
		if point.EventType == "tool.response" {
			selected = point
			break
		}
	}
	if selected.ID == "" {
		t.Fatal("tool checkpoint missing")
	}
	created, err := fork.Create(ctx, source, source, selected, manifest, fork.Options{})
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Store: source, Dispatch: &dispatch.Dispatcher{Store: source, Manifest: manifest}, Manifest: manifest, Provider: agent.ScriptedProvider{}}
	child, err := runner.Execute(ctx, created.Run.ID, 20)
	if err != nil || child.Status != "completed" {
		t.Fatalf("child=%+v err=%v", child, err)
	}
	report, err := Verify(ctx, source, child.ID, manifest)
	if err != nil || !report.Verified {
		t.Fatalf("fork replay=%+v err=%v", report, err)
	}
}

func TestVerifyForkRejectsTamperedLineageAndParentPrefix(t *testing.T) {
	ctx := context.Background()
	t.Run("lineage", func(t *testing.T) {
		source, _, child, manifest := completedFork(t)
		if _, err := source.DB.ExecContext(ctx, "UPDATE fork_lineage SET prefix_digest='tampered' WHERE child_run_id=?", child.ID); err != nil {
			t.Fatal(err)
		}
		if report, err := Verify(ctx, source, child.ID, manifest); err == nil && report.Verified {
			t.Fatal("tampered lineage accepted")
		}
	})
	t.Run("parent prefix", func(t *testing.T) {
		source, parent, child, manifest := completedFork(t)
		if _, err := source.DB.ExecContext(ctx, "UPDATE events SET payload='{}' WHERE run_id=? AND seq=1", parent.ID); err != nil {
			t.Fatal(err)
		}
		if report, err := Verify(ctx, source, child.ID, manifest); err == nil && report.Verified {
			t.Fatal("tampered parent accepted")
		}
	})
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

type companyProvider struct{ scenario string }

func (p companyProvider) Next(_ context.Context, _ string, history []agent.Message, _ []compiler.Operation) (agent.Message, error) {
	tools := []agent.Message{}
	for _, message := range history {
		if message.Role == "tool" {
			tools = append(tools, message)
		}
	}
	call := func(id, operation string, args map[string]any) (agent.Message, error) {
		return agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: id, OperationID: operation, Arguments: args}}}, nil
	}
	switch len(tools) {
	case 0:
		return call("company-note", "crmAddAccountNote", map[string]any{"account_id": "A-104", "body": "Reviewed the billing result."})
	case 1:
		if p.scenario == "company-incident" {
			return call("company-issue", "ticketCreateIssue", map[string]any{"project_id": "PROJ-ENG", "account_id": "A-104", "title": "Billing retry incident", "priority": "high"})
		}
	case 2:
		if p.scenario == "company-incident" {
			var issue struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(tools[1].Content), &issue); err != nil {
				return agent.Message{}, err
			}
			return call("company-comment", "ticketAddComment", map[string]any{"issue_id": issue.ID, "body": "Investigating retry worker."})
		}
	case 3:
		if p.scenario == "company-incident" {
			return call("company-message", "messagePostMessage", map[string]any{"channel_id": "CH-SUPPORT", "body": "Incident ticket is open."})
		}
	}
	return agent.Message{Role: "assistant", Content: "Review complete."}, nil
}

func completedCompanyRun(t *testing.T, scenario string) (*store.Store, store.Run, compiler.Manifest) {
	t.Helper()
	ctx := context.Background()
	root := filepath.Join("..", "..", "examples", "company")
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
	source, err := store.Open(filepath.Join(t.TempDir(), "company.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	world, err := source.SeedScenario(ctx, 42, manifest.Digest, scenario)
	if err != nil {
		t.Fatal(err)
	}
	run, err := source.CreateRun(ctx, world.ID, scenario, "scripted-test", "fixture-v1", "Review C-104 billing and incident signals.", "")
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Store: source, Dispatch: &dispatch.Dispatcher{Store: source, Manifest: manifest}, Manifest: manifest, Provider: companyProvider{scenario}}
	done, err := runner.Execute(ctx, run.ID, 12)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "completed" {
		t.Fatalf("run status=%s", done.Status)
	}
	return source, done, manifest
}

func TestVerifyCompanyScenariosPreserveSource(t *testing.T) {
	for _, scenario := range []string{"company-incident", "company-routine", "company-no-duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			source, run, manifest := completedCompanyRun(t, scenario)
			before, err := source.Events(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			var notesBefore int
			if err = source.DB.QueryRowContext(ctx, "SELECT count(*) FROM crm_notes WHERE world_id=?", run.WorldID).Scan(&notesBefore); err != nil {
				t.Fatal(err)
			}
			report, err := Verify(ctx, source, run.ID, manifest)
			if err != nil {
				t.Fatal(err)
			}
			if !report.Verified || report.ModelTurns == 0 || report.ToolCalls == 0 {
				t.Fatalf("report=%+v", report)
			}
			after, err := source.Events(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			var notesAfter int
			if err = source.DB.QueryRowContext(ctx, "SELECT count(*) FROM crm_notes WHERE world_id=?", run.WorldID).Scan(&notesAfter); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) || notesBefore != notesAfter {
				t.Fatalf("source changed during replay: events=%d/%d notes=%d/%d", len(before), len(after), notesBefore, notesAfter)
			}
		})
	}
}

func TestVerifyDetectsTamperedCompanyTables(t *testing.T) {
	for _, tc := range []struct{ table, update string }{
		{"subscriptions", "UPDATE subscriptions SET plan='tampered' WHERE world_id=?"},
		{"crm_accounts", "UPDATE crm_accounts SET status='tampered' WHERE world_id=?"},
		{"crm_contacts", "UPDATE crm_contacts SET name='tampered' WHERE world_id=?"},
		{"crm_notes", "UPDATE crm_notes SET body='tampered' WHERE world_id=?"},
		{"ticket_projects", "UPDATE ticket_projects SET name='tampered' WHERE world_id=?"},
		{"ticket_issues", "UPDATE ticket_issues SET title='tampered' WHERE world_id=?"},
		{"ticket_comments", "UPDATE ticket_comments SET body='tampered' WHERE world_id=?"},
		{"message_workspaces", "UPDATE message_workspaces SET name='tampered' WHERE world_id=?"},
		{"message_channels", "UPDATE message_channels SET name='tampered' WHERE world_id=?"},
		{"message_members", "UPDATE message_members SET principal_id='tampered' WHERE world_id=?"},
		{"message_messages", "UPDATE message_messages SET body='tampered' WHERE world_id=?"},
	} {
		t.Run(tc.table, func(t *testing.T) {
			ctx := context.Background()
			source, run, manifest := completedCompanyRun(t, "company-incident")
			result, err := source.DB.ExecContext(ctx, tc.update, run.WorldID)
			if err != nil {
				t.Fatal(err)
			}
			changed, err := result.RowsAffected()
			if err != nil || changed == 0 {
				t.Fatalf("tamper changed %d rows: %v", changed, err)
			}
			report, err := Verify(ctx, source, run.ID, manifest)
			if err != nil {
				t.Fatal(err)
			}
			if report.Verified || report.Divergence != tc.table+" state differs" {
				t.Fatalf("tampered %s accepted: %+v", tc.table, report)
			}
		})
	}
}

func TestVerifyBillingRunIgnoresCompanyTables(t *testing.T) {
	ctx := context.Background()
	source, run, manifest := completedRun(t)
	if _, err := source.DB.ExecContext(ctx, "INSERT INTO crm_accounts(world_id,id,customer_id,status,representative_id) VALUES(?,?,?,?,?)", run.WorldID, "A-extra", "C-104", "active", "REP-1"); err != nil {
		t.Fatal(err)
	}
	report, err := Verify(ctx, source, run.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified {
		t.Fatalf("billing replay included company table: %+v", report)
	}
}
