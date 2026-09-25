package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"twinwright/internal/chaos"
	"twinwright/internal/store"
)

func chaosRun(t *testing.T, rule string) (*Dispatcher, *store.Store, store.Run) {
	t.Helper()
	d, s, existing := setup(t, "")
	input := []byte("version: 1\nrules:\n  - id: target\n    " + rule + "\n")
	policy, err := chaos.Parse(input, d.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := policy.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRunWithChaos(context.Background(), existing.WorldID, "duplicate-charge", "scripted", "fixture-v1", "task", "", encoded, policy.Digest())
	if err != nil {
		t.Fatal(err)
	}
	return d, s, run
}

func TestPreExecutionChaosFaults(t *testing.T) {
	cases := []struct {
		name, rule string
		statuses   []int
	}{
		{"http", "type: http_error\n    operations: [getCharge]\n    status: 503\n    times: 1", []int{503, 200}},
		{"timeout", "type: timeout\n    operations: [getCharge]\n    times: 1", []int{0, 200}},
		{"rate", "type: rate_limit\n    operations: [getCharge]\n    after_calls: 3", []int{200, 200, 200, 429}},
		{"permission", "type: permission_revocation\n    operations: [getCharge]\n    after_calls: 1", []int{200, 403}},
		{"latency", "type: latency\n    operations: [getCharge]\n    duration_ms: 5000", []int{200, 200}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, s, run := chaosRun(t, tc.rule)
			ctx := context.Background()
			for i, expected := range tc.statuses {
				result, err := d.Invoke(ctx, run.ID, fmt.Sprintf("call-%d", i), "getCharge", map[string]any{"id": "CH-1002"})
				if err != nil || result.Status != expected {
					t.Fatalf("call %d status=%d want=%d err=%v", i, result.Status, expected, err)
				}
			}
			events, err := s.Events(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			faults := 0
			for _, event := range events {
				if event.Type == "chaos.injected" {
					faults++
					if tc.name == "latency" && !json.Valid(event.Payload) {
						t.Fatal("invalid latency event")
					}
				}
			}
			wantFaults := 1
			if tc.name == "latency" {
				wantFaults = 2
			}
			if faults != wantFaults {
				t.Fatalf("injected events=%d want=%d", faults, wantFaults)
			}
		})
	}
}

func TestChaosFaultIsIdempotentAndInvalidArgsDoNotConsumeIt(t *testing.T) {
	d, s, run := chaosRun(t, "type: http_error\n    operations: [createRefund]\n    status: 503\n    times: 1")
	ctx := context.Background()
	valid := map[string]any{"charge_id": "CH-1002", "amount_cents": 1000, "reason": "duplicate"}
	bad, err := d.Invoke(ctx, run.ID, "bad", "createRefund", map[string]any{"charge_id": "CH-1002"})
	if err != nil || bad.Status != 400 {
		t.Fatalf("bad=%+v err=%v", bad, err)
	}
	first, err := d.Invoke(ctx, run.ID, "first", "createRefund", valid)
	if err != nil || first.Status != 503 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	again, err := d.Invoke(ctx, run.ID, "first", "createRefund", valid)
	if err != nil || again.Status != 503 {
		t.Fatalf("again=%+v err=%v", again, err)
	}
	var calls, refunds int
	if err := s.DB.QueryRowContext(ctx, "SELECT matching_calls FROM chaos_rule_state WHERE run_id=? AND rule_id='target'", run.ID).Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", run.WorldID).Scan(&refunds); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || refunds != 0 {
		t.Fatalf("calls=%d refunds=%d", calls, refunds)
	}
}

func TestPartialServiceOutageSharesCounterAcrossOperations(t *testing.T) {
	d, _, run := chaosRun(t, "type: partial_service_outage\n    operations: [getCharge, listCharges]\n    times: 2")
	ctx := context.Background()
	for i, item := range []struct {
		operation string
		args      map[string]any
		status    int
	}{
		{"getCharge", map[string]any{"id": "CH-1002"}, 503},
		{"listCharges", map[string]any{"id": "INV-104"}, 503},
		{"getCharge", map[string]any{"id": "CH-1002"}, 200},
	} {
		result, err := d.Invoke(ctx, run.ID, fmt.Sprintf("outage-%d", i), item.operation, item.args)
		if err != nil || result.Status != item.status {
			t.Fatalf("call %d status=%d err=%v", i, result.Status, err)
		}
	}
}

func TestTimeoutAfterCommitHidesSuccessfulRefund(t *testing.T) {
	d, s, run := chaosRun(t, "type: timeout_after_commit\n    operations: [createRefund]\n    times: 1")
	ctx := context.Background()
	args := map[string]any{"charge_id": "CH-1002", "amount_cents": 500, "reason": "partial duplicate refund"}
	first, err := d.Invoke(ctx, run.ID, "refund-first", "createRefund", args)
	if err != nil || first.Status != 0 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	var refunds int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", run.WorldID).Scan(&refunds); err != nil || refunds != 1 {
		t.Fatalf("refunds=%d err=%v", refunds, err)
	}
	again, err := d.Invoke(ctx, run.ID, "refund-first", "createRefund", args)
	if err != nil || again.Status != 0 {
		t.Fatalf("same call=%+v err=%v", again, err)
	}
	second, err := d.Invoke(ctx, run.ID, "refund-second", "createRefund", args)
	if err != nil || second.Status != 201 {
		t.Fatalf("new call=%+v err=%v", second, err)
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", run.WorldID).Scan(&refunds); err != nil || refunds != 2 {
		t.Fatalf("refunds=%d err=%v", refunds, err)
	}
	var hiddenStatus int
	if err := s.DB.QueryRowContext(ctx, "SELECT status FROM chaos_hidden_outcomes WHERE run_id=? AND call_id='refund-first'", run.ID).Scan(&hiddenStatus); err != nil || hiddenStatus != 201 {
		t.Fatalf("hidden=%d err=%v", hiddenStatus, err)
	}
}

func TestTimeoutAfterCommitRollsBackWithAuditFailure(t *testing.T) {
	d, s, run := chaosRun(t, "type: timeout_after_commit\n    operations: [createRefund]\n    times: 1")
	ctx := context.Background()
	if _, err := s.DB.ExecContext(ctx, `CREATE TRIGGER fail_hidden BEFORE INSERT ON chaos_hidden_outcomes BEGIN SELECT RAISE(ABORT, 'audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := d.Invoke(ctx, run.ID, "refund-first", "createRefund", map[string]any{"charge_id": "CH-1002", "amount_cents": 500, "reason": "partial"})
	if err == nil {
		t.Fatal("audit failure did not roll back")
	}
	var refunds, states int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", run.WorldID).Scan(&refunds); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM chaos_rule_state WHERE run_id=?", run.ID).Scan(&states); err != nil {
		t.Fatal(err)
	}
	if refunds != 0 || states != 0 {
		t.Fatalf("partial commit: refunds=%d states=%d", refunds, states)
	}
}

func TestMalformedResponseStoresRealResultButReturnsInvalidShape(t *testing.T) {
	d, s, run := chaosRun(t, "type: malformed_response\n    operations: [getCharge]\n    body: '{broken'\n    times: 1")
	result, err := d.Invoke(context.Background(), run.ID, "read", "getCharge", map[string]any{"id": "CH-1002"})
	if err != nil || result.Status != 200 || string(result.Body) != `"{broken"` {
		t.Fatalf("visible=%+v err=%v", result, err)
	}
	var hidden string
	if err := s.DB.QueryRow("SELECT body FROM chaos_hidden_outcomes WHERE run_id=? AND call_id='read'", run.ID).Scan(&hidden); err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(hidden)) || hidden == string(result.Body) {
		t.Fatalf("hidden=%s visible=%s", hidden, result.Body)
	}
}

func TestStaleReadIsScopedToArguments(t *testing.T) {
	d, s, run := chaosRun(t, "type: stale_read\n    operations: [getCharge]\n    after_calls: 1")
	ctx := context.Background()
	first, err := d.Invoke(ctx, run.ID, "read-1", "getCharge", map[string]any{"id": "CH-1002"})
	if err != nil || first.Status != 200 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err := d.Invoke(ctx, run.ID, "refund", "createRefund", map[string]any{"charge_id": "CH-1002", "amount_cents": 500, "reason": "partial"}); err != nil {
		t.Fatal(err)
	}
	stale, err := d.Invoke(ctx, run.ID, "read-2", "getCharge", map[string]any{"id": "CH-1002"})
	if err != nil || string(stale.Body) != string(first.Body) {
		t.Fatalf("stale=%+v first=%+v err=%v", stale, first, err)
	}
	other, err := d.Invoke(ctx, run.ID, "read-3", "getCharge", map[string]any{"id": "CH-1001"})
	if err != nil || other.Status != 200 || string(other.Body) == string(first.Body) {
		t.Fatalf("other=%+v err=%v", other, err)
	}
	var refunded int
	if err := s.DB.QueryRowContext(ctx, "SELECT refunded_cents FROM charges WHERE world_id=? AND id='CH-1002'", run.WorldID).Scan(&refunded); err != nil || refunded != 500 {
		t.Fatalf("refunded=%d err=%v", refunded, err)
	}
}

func TestConcurrentActorMutatesBeforeAgentRead(t *testing.T) {
	d, s, run := chaosRun(t, "type: concurrent_mutation\n    operations: [getCharge]\n    times: 1\n    actor:\n      operation: createRefund\n      arguments: {charge_id: CH-1002, amount_cents: 500, reason: actor}")
	result, err := d.Invoke(context.Background(), run.ID, "read", "getCharge", map[string]any{"id": "CH-1002"})
	if err != nil || result.Status != 200 {
		t.Fatalf("read=%+v err=%v", result, err)
	}
	var charge struct {
		Refunded int `json:"refunded_cents"`
	}
	if err := json.Unmarshal(result.Body, &charge); err != nil || charge.Refunded != 500 {
		t.Fatalf("charge=%+v err=%v", charge, err)
	}
	var actorEvents int
	if err := s.DB.QueryRow("SELECT count(*) FROM events WHERE run_id=? AND type='chaos.actor_mutation'", run.ID).Scan(&actorEvents); err != nil || actorEvents != 1 {
		t.Fatalf("actor events=%d err=%v", actorEvents, err)
	}
}
