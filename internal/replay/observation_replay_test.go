package replay

import (
	"context"
	"encoding/json"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/checkpoint"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/fork"
	"twinwright/internal/store"
)

// overriddenFork forks the billing run after its successful listCharges call,
// swaps the charge order the agent sees, and completes the child.
func overriddenFork(t *testing.T) (*store.Store, store.Run, compiler.Manifest) {
	t.Helper()
	ctx := context.Background()
	source, parent, manifest := completedRun(t)
	points, err := checkpoint.List(ctx, source, parent.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	events, err := source.Events(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	var selected checkpoint.Checkpoint
	var callID string
	for _, point := range points {
		var response struct {
			CallID      string `json:"call_id"`
			OperationID string `json:"operation_id"`
			Status      int    `json:"status"`
		}
		if point.EventType != "tool.response" || json.Unmarshal(events[point.EventSeq-1].Payload, &response) != nil {
			continue
		}
		if response.OperationID == "listCharges" && response.Status == 200 {
			selected, callID = point, response.CallID
		}
	}
	if callID == "" {
		t.Fatal("successful listCharges checkpoint missing")
	}
	var body string
	if err := source.DB.QueryRowContext(ctx, "SELECT body FROM tool_results WHERE run_id=? AND call_id=?", parent.ID, callID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var charges []json.RawMessage
	if err := json.Unmarshal([]byte(body), &charges); err != nil || len(charges) != 2 {
		t.Fatalf("charges=%s err=%v", body, err)
	}
	swapped, err := json.Marshal([]json.RawMessage{charges[1], charges[0]})
	if err != nil {
		t.Fatal(err)
	}
	created, err := fork.Create(ctx, source, source, selected, manifest, fork.Options{Observation: &store.Observation{CallID: callID, Status: 200, Body: swapped}})
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Store: source, Dispatch: &dispatch.Dispatcher{Store: source, Manifest: manifest}, Manifest: manifest, Provider: agent.ScriptedProvider{}}
	child, err := runner.Execute(ctx, created.Run.ID, 20)
	if err != nil || child.Status != "completed" {
		t.Fatalf("child=%+v err=%v", child, err)
	}
	return source, child, manifest
}

func TestVerifyForkWithObservationOverride(t *testing.T) {
	source, child, manifest := overriddenFork(t)
	report, err := Verify(context.Background(), source, child.ID, manifest)
	if err != nil || !report.Verified {
		t.Fatalf("override fork replay=%+v err=%v", report, err)
	}
}

func TestVerifyForkRejectsTamperedObservationOverride(t *testing.T) {
	ctx := context.Background()
	tampering := map[string]string{
		"override row body":  "UPDATE fork_observations SET body='[]' WHERE child_run_id=?",
		"override row gone":  "DELETE FROM fork_observations WHERE child_run_id=?",
		"override event":     "UPDATE events SET payload=json_set(payload,'$.status',500) WHERE run_id=? AND type='observation.overridden'",
		"saved child result": "UPDATE tool_results SET body='[]' WHERE run_id=? AND status=200 AND operation_id='listCharges'",
	}
	for name, statement := range tampering {
		t.Run(name, func(t *testing.T) {
			source, child, manifest := overriddenFork(t)
			result, err := source.DB.ExecContext(ctx, statement, child.ID)
			if err != nil {
				t.Fatal(err)
			}
			if n, err := result.RowsAffected(); err != nil || n != 1 {
				t.Fatalf("tampering changed %d rows err=%v", n, err)
			}
			if report, err := Verify(ctx, source, child.ID, manifest); err == nil && report.Verified {
				t.Fatal("tampered override accepted")
			}
		})
	}
}
