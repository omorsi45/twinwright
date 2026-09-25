package dispatch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"twinwright/internal/compiler"
	"twinwright/internal/store"
)

func setup(t *testing.T, fault string) (*Dispatcher, *store.Store, store.Run) {
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
	s, err := store.Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	w, err := s.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, w.ID, "duplicate-charge", "scripted", "fixture-v1", "task", fault)
	if err != nil {
		t.Fatal(err)
	}
	return &Dispatcher{Store: s, Manifest: manifest}, s, run
}

func TestRefundMutatesStateAndRepeatedCallIsSafe(t *testing.T) {
	d, s, r := setup(t, "")
	ctx := context.Background()
	before, err := d.Invoke(ctx, r.ID, "get-1", "getCharge", map[string]any{"id": "CH-1002"})
	if err != nil {
		t.Fatal(err)
	}
	var charge struct {
		AmountCents   int64 `json:"amount_cents"`
		RefundedCents int64 `json:"refunded_cents"`
	}
	if err = json.Unmarshal(before.Body, &charge); err != nil {
		t.Fatal(err)
	}
	if before.Status != 200 || charge.RefundedCents != 0 {
		t.Fatalf("before=%+v %+v", before, charge)
	}
	args := map[string]any{"charge_id": "CH-1002", "amount_cents": charge.AmountCents, "reason": "duplicate charge"}
	first, err := d.Invoke(ctx, r.ID, "refund-1", "createRefund", args)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.Invoke(ctx, r.ID, "refund-1", "createRefund", args)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != 201 || string(first.Body) != string(second.Body) {
		t.Fatalf("responses: %+v %+v", first, second)
	}
	after, err := d.Invoke(ctx, r.ID, "get-2", "getCharge", map[string]any{"id": "CH-1002"})
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(after.Body, &charge); err != nil {
		t.Fatal(err)
	}
	if charge.RefundedCents != charge.AmountCents {
		t.Fatalf("charge after refund=%+v", charge)
	}
	var count int
	if err = s.DB.QueryRow("SELECT count(*) FROM refunds WHERE world_id=?", r.WorldID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("refund count=%d", count)
	}
}

func TestOneShotFaultThenNewCallSucceeds(t *testing.T) {
	d, s, r := setup(t, "listCharges")
	ctx := context.Background()
	args := map[string]any{"id": "INV-104"}
	a, err := d.Invoke(ctx, r.ID, "call-a", "listCharges", args)
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := d.Invoke(ctx, r.ID, "call-a", "listCharges", args)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.Invoke(ctx, r.ID, "call-b", "listCharges", args)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != 503 || repeat.Status != 503 || b.Status != 200 {
		t.Fatalf("statuses=%d,%d,%d", a.Status, repeat.Status, b.Status)
	}
	events, err := s.Events(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	retries := 0
	for _, e := range events {
		if e.Type == "retry" {
			retries++
		}
	}
	if retries != 1 {
		t.Fatalf("retry events=%d", retries)
	}
}

func TestCallIDCannotBeReusedWithDifferentArguments(t *testing.T) {
	d, _, r := setup(t, "")
	ctx := context.Background()
	if _, err := d.Invoke(ctx, r.ID, "same", "getCharge", map[string]any{"id": "CH-1001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Invoke(ctx, r.ID, "same", "getCharge", map[string]any{"id": "CH-1002"}); err == nil {
		t.Fatal("changed arguments returned a saved result")
	}
}

func TestRefundRollbackAndIdempotentCommit(t *testing.T) {
	d, s, r := setup(t, "")
	ctx := context.Background()
	var amount int64
	if err := s.DB.QueryRow("SELECT amount_cents FROM charges WHERE world_id=? AND id='CH-1002'", r.WorldID).Scan(&amount); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"charge_id": "CH-1002", "amount_cents": amount, "reason": "duplicate charge"}
	transcript := `[{"role":"assistant","tool_calls":[{"id":"refund-call","operation_id":"createRefund","arguments":{"charge_id":"CH-1002","amount_cents":` + strconv.FormatInt(amount, 10) + `,"reason":"duplicate charge"}}]}]`
	if err := s.StartModelCall(ctx, r.ID, map[string]any{"task": "refund"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTurn(ctx, r.ID, 1, transcript, map[string]any{"tool_call": "refund-call"}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Events(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`CREATE TRIGGER fail_tool_result BEFORE INSERT ON tool_results BEGIN SELECT RAISE(ABORT, 'inject failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Invoke(ctx, r.ID, "refund-call", "createRefund", args); err == nil {
		t.Fatal("expected transaction failure")
	}
	var refunded, refundCount int64
	if err = s.DB.QueryRow("SELECT refunded_cents FROM charges WHERE world_id=? AND id='CH-1002'", r.WorldID).Scan(&refunded); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow("SELECT count(*) FROM refunds WHERE world_id=?", r.WorldID).Scan(&refundCount); err != nil {
		t.Fatal(err)
	}
	failedRun, err := s.Run(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterFailure, err := s.Events(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refunded != 0 || refundCount != 0 || failedRun.Transcript != transcript || len(afterFailure) != len(before) {
		t.Fatalf("rollback failed: refunded=%d refunds=%d transcript=%s events=%d/%d", refunded, refundCount, failedRun.Transcript, len(afterFailure), len(before))
	}
	if _, err = s.DB.Exec("DROP TRIGGER fail_tool_result"); err != nil {
		t.Fatal(err)
	}
	first, err := d.Invoke(ctx, r.ID, "refund-call", "createRefund", args)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != 201 {
		t.Fatalf("status=%d", first.Status)
	}
	committed, err := s.Events(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := d.Invoke(ctx, r.ID, "refund-call", "createRefund", args)
	if err != nil {
		t.Fatal(err)
	}
	finalEvents, err := s.Events(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(repeat.Body) != string(first.Body) || len(finalEvents) != len(committed) {
		t.Fatalf("repeat changed result or ledger")
	}
	if err = s.DB.QueryRow("SELECT count(*) FROM refunds WHERE world_id=?", r.WorldID).Scan(&refundCount); err != nil {
		t.Fatal(err)
	}
	if refundCount != 1 {
		t.Fatalf("refunds=%d", refundCount)
	}
}

func TestValidationErrorIsStableForMultipleInvalidArguments(t *testing.T) {
	op := compiler.Operation{Properties: map[string]string{"alpha": "string", "zeta": "string"}}
	args := map[string]any{"zeta": "", "alpha": ""}
	for i := 0; i < 100; i++ {
		status, body := validate(op, args)
		if status != 400 || body.(map[string]string)["error"] != "invalid alpha" {
			t.Fatalf("iteration %d returned HTTP %d %v", i, status, body)
		}
	}
}

func TestMessagingDispatchFaultThenSingleCommittedPost(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	world, err := s.SeedScenario(ctx, 42, "digest", "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "company-incident", "scripted", "fixture-v1", "task", "messagePostMessage")
	if err != nil {
		t.Fatal(err)
	}
	manifest := compiler.Manifest{Operations: []compiler.Operation{
		{ID: "messagePostMessage", Behavior: "messaging.postMessage", Required: []string{"body", "channel_id"}, Properties: map[string]string{"body": "string", "channel_id": "string"}},
		{ID: "messageReadChannel", Behavior: "messaging.readChannel", Required: []string{"id"}, Properties: map[string]string{"id": "string"}},
	}}
	d := Dispatcher{Store: s, Manifest: manifest}
	args := map[string]any{"channel_id": "CH-SUPPORT", "body": "Investigating C-104"}
	faulted, err := d.Invoke(ctx, run.ID, "call-1", "messagePostMessage", args)
	if err != nil || faulted.Status != 503 {
		t.Fatalf("faulted post: result=%v err=%v", faulted, err)
	}
	var count int
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM message_messages WHERE world_id=?", world.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("fault mutated state: count=%d err=%v", count, err)
	}
	posted, err := d.Invoke(ctx, run.ID, "call-2", "messagePostMessage", args)
	if err != nil || posted.Status != 201 {
		t.Fatalf("post: result=%v err=%v", posted, err)
	}
	repeat, err := d.Invoke(ctx, run.ID, "call-2", "messagePostMessage", args)
	if err != nil || string(repeat.Body) != string(posted.Body) {
		t.Fatalf("repeat: result=%v err=%v", repeat, err)
	}
	if _, err = d.Invoke(ctx, run.ID, "call-2", "messagePostMessage", map[string]any{"channel_id": "CH-SUPPORT", "body": "changed"}); err == nil {
		t.Fatal("changed arguments reused committed call ID")
	}
	read, err := d.Invoke(ctx, run.ID, "call-3", "messageReadChannel", map[string]any{"id": "CH-SUPPORT"})
	if err != nil || read.Status != 200 {
		t.Fatalf("read: result=%v err=%v", read, err)
	}
	var body struct {
		Messages []struct {
			Body string `json:"body"`
		} `json:"messages"`
	}
	if err = json.Unmarshal(read.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 1 || body.Messages[0].Body != "Investigating C-104" {
		t.Fatalf("messages=%v", body.Messages)
	}
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM message_messages WHERE world_id=?", world.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("committed count=%d err=%v", count, err)
	}
}

func TestTicketDispatchCommitsOnceAndReadsCurrentIssue(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/world.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	world, err := s.SeedScenario(ctx, 42, "digest", "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "company-incident", "scripted", "fixture-v1", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	manifest := compiler.Manifest{Operations: []compiler.Operation{
		{ID: "ticketCreateIssue", Behavior: "ticket.createIssue", Required: []string{"account_id", "priority", "project_id", "title"}, Properties: map[string]string{"account_id": "string", "priority": "string", "project_id": "string", "title": "string"}},
		{ID: "ticketGetIssue", Behavior: "ticket.getIssue", Required: []string{"id"}, Properties: map[string]string{"id": "string"}},
	}}
	d := Dispatcher{Store: s, Manifest: manifest}
	args := map[string]any{"account_id": "A-104", "priority": "high", "project_id": "PROJ-ENG", "title": "C-104 billing retry incident"}
	created, err := d.Invoke(ctx, run.ID, "issue-1", "ticketCreateIssue", args)
	if err != nil || created.Status != 201 {
		t.Fatalf("create: result=%v err=%v", created, err)
	}
	repeat, err := d.Invoke(ctx, run.ID, "issue-1", "ticketCreateIssue", args)
	if err != nil || string(repeat.Body) != string(created.Body) {
		t.Fatalf("repeat: result=%v err=%v", repeat, err)
	}
	var issue struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(created.Body, &issue); err != nil || issue.ID == "" {
		t.Fatalf("issue=%v err=%v", issue, err)
	}
	got, err := d.Invoke(ctx, run.ID, "issue-2", "ticketGetIssue", map[string]any{"id": issue.ID})
	if err != nil || got.Status != 200 {
		t.Fatalf("get: result=%v err=%v", got, err)
	}
	var count int
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM ticket_issues WHERE world_id=?", world.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("issue count=%d err=%v", count, err)
	}
}
