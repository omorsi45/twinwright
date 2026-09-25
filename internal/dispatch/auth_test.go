package dispatch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"twinwright/internal/authz"
	"twinwright/internal/chaos"
	"twinwright/internal/store"
)

func securedRun(t *testing.T, authYAML, chaosRule string) (*Dispatcher, *store.Store, store.Run) {
	t.Helper()
	d, s, existing := setup(t, "")
	policy, err := authz.Parse([]byte(authYAML), d.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := policy.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	opts := store.RunOptions{AuthJSON: encoded, AuthDigest: policy.Digest()}
	if chaosRule != "" {
		chaosPolicy, err := chaos.Parse([]byte("version: 1\nrules:\n  - id: target\n    "+chaosRule+"\n"), d.Manifest)
		if err != nil {
			t.Fatal(err)
		}
		if opts.ChaosJSON, err = chaosPolicy.CanonicalJSON(); err != nil {
			t.Fatal(err)
		}
		opts.ChaosDigest = chaosPolicy.Digest()
	}
	run, err := s.CreateRunConfigured(context.Background(), existing.WorldID, "duplicate-charge", "scripted", "fixture-v1", "task", opts)
	if err != nil {
		t.Fatal(err)
	}
	return d, s, run
}

func eventTypes(t *testing.T, s *store.Store, runID string) []string {
	t.Helper()
	events, err := s.Events(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, event := range events {
		out = append(out, event.Type)
	}
	return out
}

func count(t *testing.T, s *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUnauthorizedRefundIsDeniedBeforeChaosAndHandler(t *testing.T) {
	d, s, run := securedRun(t, "version: 1\nprincipal: {id: support}\npermissions: {allow: [charges.read]}\n", "type: http_error\n    operations: [createRefund]\n    status: 503\n    times: 1")
	ctx := context.Background()
	refund := map[string]any{"charge_id": "CH-1002", "amount_cents": 1000, "reason": "duplicate"}
	result, err := d.Invoke(ctx, run.ID, "refund-1", "createRefund", refund)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	if err := json.Unmarshal(result.Body, &body); err != nil || result.Status != 403 || body["reason"] != authz.ReasonNotGranted {
		t.Fatalf("result=%d %s err=%v", result.Status, result.Body, err)
	}
	if got := strings.Join(eventTypes(t, s, run.ID), ","); got != "execution.started,tool.request,authorization.denied,tool.response" {
		t.Fatalf("events=%s", got)
	}
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var audit map[string]any
	if err := json.Unmarshal(events[2].Payload, &audit); err != nil {
		t.Fatal(err)
	}
	if audit["principal_id"] != "support" || audit["permission"] != "refunds.create" || audit["reason"] != authz.ReasonNotGranted || audit["call_id"] != "refund-1" || audit["call_index"] != float64(1) {
		t.Fatalf("audit=%v", audit)
	}
	if n := count(t, s, "SELECT count(*) FROM refunds"); n != 0 {
		t.Fatalf("denied refund wrote %d refunds", n)
	}
	if n := count(t, s, "SELECT count(*) FROM chaos_rule_state WHERE run_id=?", run.ID); n != 0 {
		t.Fatalf("denied call advanced %d chaos rules", n)
	}
	var transcript string
	if err := s.DB.QueryRow("SELECT transcript FROM runs WHERE id=?", run.ID).Scan(&transcript); err != nil || !strings.Contains(transcript, `"status":403`) {
		t.Fatalf("transcript=%s err=%v", transcript, err)
	}

	again, err := d.Invoke(ctx, run.ID, "refund-1", "createRefund", refund)
	if err != nil || again.Status != 403 || string(again.Body) != string(result.Body) {
		t.Fatalf("reused call=%d %s err=%v", again.Status, again.Body, err)
	}
	if n := len(eventTypes(t, s, run.ID)); n != 4 {
		t.Fatalf("reused denied call appended events: %d", n)
	}
	if n := count(t, s, "SELECT call_index FROM auth_state WHERE run_id=?", run.ID); n != 1 {
		t.Fatalf("reused call advanced call index to %d", n)
	}
}

func TestAuthorizedCallIsAuditedAndStillSubjectToChaos(t *testing.T) {
	d, s, run := securedRun(t, "version: 1\nprincipal: {id: support}\npermissions: {allow: [charges.read]}\n", "type: http_error\n    operations: [getCharge]\n    status: 503\n    times: 1")
	ctx := context.Background()
	first, err := d.Invoke(ctx, run.ID, "read-1", "getCharge", map[string]any{"id": "CH-1002"})
	if err != nil || first.Status != 503 {
		t.Fatalf("first=%d err=%v", first.Status, err)
	}
	second, err := d.Invoke(ctx, run.ID, "read-2", "getCharge", map[string]any{"id": "CH-1002"})
	if err != nil || second.Status != 200 {
		t.Fatalf("second=%d err=%v", second.Status, err)
	}
	want := "execution.started,tool.request,authorization.allowed,chaos.injected,tool.response,tool.request,authorization.allowed,tool.response"
	if got := strings.Join(eventTypes(t, s, run.ID), ","); got != want {
		t.Fatalf("events=%s", got)
	}
}

func TestAuditFailureRollsBackDecisionAndMutation(t *testing.T) {
	d, s, run := securedRun(t, "version: 1\nprincipal: {id: support}\npermissions: {allow: [refunds.create]}\n", "")
	ctx := context.Background()
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_audit BEFORE INSERT ON events WHEN NEW.type='authorization.allowed' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	before := len(eventTypes(t, s, run.ID))
	if _, err := d.Invoke(ctx, run.ID, "refund-1", "createRefund", map[string]any{"charge_id": "CH-1002", "amount_cents": 1000, "reason": "duplicate"}); err == nil {
		t.Fatal("audit failure was ignored")
	}
	if n := count(t, s, "SELECT count(*) FROM refunds"); n != 0 {
		t.Fatalf("refund committed without audit: %d", n)
	}
	if n := count(t, s, "SELECT count(*) FROM auth_state"); n != 0 {
		t.Fatalf("call index committed without audit: %d", n)
	}
	if n := count(t, s, "SELECT count(*) FROM tool_results"); n != 0 {
		t.Fatalf("tool result committed without audit: %d", n)
	}
	if after := len(eventTypes(t, s, run.ID)); after != before {
		t.Fatalf("events changed from %d to %d", before, after)
	}
}

func TestInvalidArgumentsDoNotConsumeAuthorizationCall(t *testing.T) {
	d, s, run := securedRun(t, "version: 1\nprincipal: {id: support}\npermissions: {allow: [refunds.create]}\n", "")
	result, err := d.Invoke(context.Background(), run.ID, "bad", "createRefund", map[string]any{"charge_id": "CH-1002"})
	if err != nil || result.Status != 400 {
		t.Fatalf("result=%d err=%v", result.Status, err)
	}
	if n := count(t, s, "SELECT count(*) FROM auth_state"); n != 0 {
		t.Fatalf("invalid call consumed a call number")
	}
	for _, typ := range eventTypes(t, s, run.ID) {
		if strings.HasPrefix(typ, "authorization.") {
			t.Fatalf("invalid call was audited: %s", typ)
		}
	}
}

func TestHandlerInvalidArgumentsDoNotConsumeAuthorizationCall(t *testing.T) {
	d, s, run := securedRun(t, "version: 1\nprincipal: {id: support}\npermissions: {allow: [refunds.create, customers.read]}\n", "")
	ctx := context.Background()
	for i, args := range []map[string]any{
		{"charge_id": "CH-1002", "amount_cents": json.Number("1.5"), "reason": "duplicate"},
		{"charge_id": "CH-1002", "amount_cents": 1.5, "reason": "duplicate"},
		{"charge_id": "CH-1002", "amount_cents": 0, "reason": "duplicate"},
		{"charge_id": "CH-1002", "amount_cents": json.Number("1e3"), "reason": "duplicate"},
	} {
		result, err := d.Invoke(ctx, run.ID, "bad-"+string(rune('a'+i)), "createRefund", args)
		if err != nil || result.Status != 400 {
			t.Fatalf("case %d result=%d %s err=%v", i, result.Status, result.Body, err)
		}
	}
	if n := count(t, s, "SELECT count(*) FROM auth_state"); n != 0 {
		t.Fatalf("handler-invalid calls consumed a call number")
	}
	if _, err := d.Invoke(ctx, run.ID, "good", "getCustomer", map[string]any{"id": "C-104"}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "SELECT call_index FROM auth_state WHERE run_id=?", run.ID); n != 1 {
		t.Fatalf("first valid call index=%d", n)
	}
	for _, typ := range eventTypes(t, s, run.ID) {
		if typ == "authorization.denied" {
			t.Fatal("invalid call was audited as denied")
		}
	}
}

func TestUnrestrictedRunHasNoAuthorizationEvents(t *testing.T) {
	d, s, run := setup(t, "")
	if _, err := d.Invoke(context.Background(), run.ID, "read", "getCharge", map[string]any{"id": "CH-1002"}); err != nil {
		t.Fatal(err)
	}
	for _, typ := range eventTypes(t, s, run.ID) {
		if strings.HasPrefix(typ, "authorization.") {
			t.Fatalf("unrestricted run audited: %s", typ)
		}
	}
}
