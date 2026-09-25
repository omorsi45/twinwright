package fork

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/checkpoint"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

func responseCallID(t *testing.T, s *store.Store, runID string, seq int) string {
	t.Helper()
	events, err := s.Events(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		CallID string `json:"call_id"`
	}
	if err := json.Unmarshal(events[seq-1].Payload, &response); err != nil || response.CallID == "" {
		t.Fatalf("event %d is not a tool response: %s", seq, events[seq-1].Payload)
	}
	return response.CallID
}

func TestCreateAppliesObservationOverrideOnlyToChild(t *testing.T) {
	source, destination, parent, manifest := completedBilling(t)
	ctx := context.Background()
	selected := operationCheckpoint(t, source, parent.ID, manifest, "tool.response", "listCharges")
	callID := responseCallID(t, source, parent.ID, selected.EventSeq)
	amount := refundAmount(t, source, parent.WorldID)
	body := fmt.Sprintf(`[{"amount_cents":%d,"id":"CH-1002"},{"amount_cents":%d,"id":"CH-1001"}]`, amount, amount)
	var parentBody string
	if err := source.DB.QueryRowContext(ctx, "SELECT body FROM tool_results WHERE run_id=? AND call_id=?", parent.ID, callID).Scan(&parentBody); err != nil {
		t.Fatal(err)
	}
	created, err := Create(ctx, source, destination, selected, manifest, Options{Observation: &store.Observation{CallID: callID, Status: 200, Body: json.RawMessage(body)}})
	if err != nil {
		t.Fatal(err)
	}
	var messages []agent.Message
	if err := json.Unmarshal([]byte(created.Run.Transcript), &messages); err != nil {
		t.Fatal(err)
	}
	last := messages[len(messages)-1]
	if last.Role != "tool" || last.CallID != callID || last.Status != 200 || last.Content != body {
		t.Fatalf("child observation=%+v", last)
	}
	var childBody string
	var childStatus int
	if err := destination.DB.QueryRowContext(ctx, "SELECT status,body FROM tool_results WHERE run_id=? AND call_id=?", created.Run.ID, callID).Scan(&childStatus, &childBody); err != nil {
		t.Fatal(err)
	}
	if childStatus != 200 || childBody != body {
		t.Fatalf("child saved result=%d %s", childStatus, childBody)
	}
	var afterParentBody string
	if err := source.DB.QueryRowContext(ctx, "SELECT body FROM tool_results WHERE run_id=? AND call_id=?", parent.ID, callID).Scan(&afterParentBody); err != nil || afterParentBody != parentBody {
		t.Fatalf("parent saved result changed: %s err=%v", afterParentBody, err)
	}
	override, err := destination.ObservationOverride(ctx, created.Run.ID)
	if err != nil || override.CallID != callID || override.Status != 200 || string(override.Body) != body {
		t.Fatalf("recorded override=%+v err=%v", override, err)
	}
	events, err := destination.Events(ctx, created.Run.ID)
	if err != nil || len(events) != 2 || events[0].Type != "execution.forked" || events[1].Type != "observation.overridden" {
		t.Fatalf("child events=%+v err=%v", events, err)
	}
	var audit struct {
		CallID         string          `json:"call_id"`
		OperationID    string          `json:"operation_id"`
		OriginalStatus int             `json:"original_status"`
		Status         int             `json:"status"`
		Body           json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(events[1].Payload, &audit); err != nil {
		t.Fatal(err)
	}
	if audit.CallID != callID || audit.OperationID != "listCharges" || audit.OriginalStatus != 200 || audit.Status != 200 || string(audit.Body) != body {
		t.Fatalf("override event=%s", events[1].Payload)
	}
	runner := agent.Runner{Store: destination, Dispatch: &dispatch.Dispatcher{Store: destination, Manifest: manifest}, Manifest: manifest, Provider: agent.ScriptedProvider{}}
	if child, err := runner.Execute(ctx, created.Run.ID, 5); err != nil || child.Status != "completed" {
		t.Fatalf("child=%+v err=%v", child, err)
	}
	var charge string
	if err := destination.DB.QueryRowContext(ctx, "SELECT charge_id FROM refunds WHERE world_id=?", created.Run.WorldID).Scan(&charge); err != nil || charge != "CH-1001" {
		t.Fatalf("child refunded %q err=%v; the overridden observation was not what the agent saw", charge, err)
	}
	if err := destination.DB.QueryRowContext(ctx, "SELECT charge_id FROM refunds WHERE world_id=?", parent.WorldID).Scan(&charge); err != nil || charge != "CH-1002" {
		t.Fatalf("parent refund changed to %q err=%v", charge, err)
	}
}

func TestCreateRejectsInvalidObservationOverride(t *testing.T) {
	source, destination, parent, manifest := completedBilling(t)
	ctx := context.Background()
	listed := operationCheckpoint(t, source, parent.ID, manifest, "tool.response", "listCharges")
	callID := responseCallID(t, source, parent.ID, listed.EventSeq)
	decision := operationCheckpoint(t, source, parent.ID, manifest, "model.response", "createRefund")
	cases := map[string]struct {
		point       checkpoint.Checkpoint
		observation store.Observation
	}{
		"model response checkpoint": {decision, store.Observation{CallID: callID, Status: 200, Body: json.RawMessage(`[]`)}},
		"other call":                {listed, store.Observation{CallID: "fixture-1", Status: 200, Body: json.RawMessage(`[]`)}},
		"missing call":              {listed, store.Observation{Status: 200, Body: json.RawMessage(`[]`)}},
		"status too high":           {listed, store.Observation{CallID: callID, Status: 600, Body: json.RawMessage(`[]`)}},
		"negative status":           {listed, store.Observation{CallID: callID, Status: -1, Body: json.RawMessage(`[]`)}},
		"invalid body":              {listed, store.Observation{CallID: callID, Status: 200, Body: json.RawMessage(`{`)}},
		"empty body":                {listed, store.Observation{CallID: callID, Status: 200}},
		"two values":                {listed, store.Observation{CallID: callID, Status: 200, Body: json.RawMessage(`1 2`)}},
	}
	for name, tc := range cases {
		observation := tc.observation
		if _, err := Create(ctx, source, destination, tc.point, manifest, Options{Observation: &observation}); err == nil {
			t.Errorf("%s: override accepted", name)
		}
	}
	var count int
	if err := destination.DB.QueryRowContext(ctx, "SELECT count(*) FROM fork_lineage").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid overrides wrote %d forks", count)
	}
}
