package authz

import (
	"context"
	"testing"

	"twinwright/internal/compiler"
	"twinwright/internal/store"
)

type securedRun struct {
	store    *store.Store
	run      store.Run
	manifest compiler.Manifest
}

func newSecuredRun(t *testing.T, policyYAML string) securedRun {
	t.Helper()
	ctx := context.Background()
	manifest := manifestFrom(t, "company")
	s, err := store.Open(t.TempDir() + "/authz.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	world, err := s.SeedScenario(ctx, 42, manifest.Digest, "company-incident")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"INSERT INTO ticket_issues VALUES(?, 'ISS-104', 'PROJ-ENG', 'A-104', 'C-104 duplicate charge', 'open', 'high')",
		"INSERT INTO ticket_issues VALUES(?, 'ISS-205', 'PROJ-ENG', 'A-205', 'C-205 billing question', 'open', 'low')",
	} {
		if _, err := s.DB.ExecContext(ctx, stmt, world.ID); err != nil {
			t.Fatal(err)
		}
	}
	opts := store.RunOptions{}
	if policyYAML != "" {
		policy, err := Parse([]byte(policyYAML), manifest)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := policy.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		opts.AuthJSON, opts.AuthDigest = encoded, policy.Digest()
	}
	run, err := s.CreateRunConfigured(ctx, world.ID, "company-incident", "scripted", "fixture-v1", "task", opts)
	if err != nil {
		t.Fatal(err)
	}
	return securedRun{s, run, manifest}
}

func (r securedRun) decide(t *testing.T, operationID string, args map[string]any) Decision {
	t.Helper()
	ctx := context.Background()
	op := r.manifest.Operation(operationID)
	if op == nil {
		t.Fatalf("unknown operation %s", operationID)
	}
	tx, err := r.store.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	decision, err := Decide(ctx, tx, r.run.ID, r.run.WorldID, *op, args)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return decision
}

type step struct {
	operation string
	args      map[string]any
	allowed   bool
	reason    string
}

func (r securedRun) expect(t *testing.T, steps []step) {
	t.Helper()
	for i, s := range steps {
		got := r.decide(t, s.operation, s.args)
		if !got.Enforced || got.Allowed != s.allowed || got.Reason != s.reason || got.Call != i+1 {
			t.Errorf("step %d %s %v: got %+v, want allowed=%v reason=%q call=%d", i+1, s.operation, s.args, got, s.allowed, s.reason, i+1)
		}
	}
}

func id(value string) map[string]any { return map[string]any{"id": value} }

func TestDecideUnionsRoleAndDirectGrantsAndDeniesByDefault(t *testing.T) {
	r := newSecuredRun(t, "version: 1\nprincipal: {id: support, roles: [reader]}\nroles: {reader: {allow: [customers.read]}}\npermissions: {allow: [charges.read]}\n")
	r.expect(t, []step{
		{"getCustomer", id("C-104"), true, ""},
		{"getCharge", id("CH-1001"), true, ""},
		{"listInvoices", id("C-104"), false, ReasonNotGranted},
		{"createRefund", map[string]any{"charge_id": "CH-1002", "amount_cents": 100, "reason": "dup"}, false, ReasonNotGranted},
	})
	if got := r.decide(t, "getCustomer", id("C-104")); got.Principal != "support" || got.Permission != "customers.read" {
		t.Fatalf("decision=%+v", got)
	}
}

func TestDecideAppliesTemporaryGrantsAndRevocationsByCallNumber(t *testing.T) {
	r := newSecuredRun(t, "version: 1\nprincipal: {id: support}\npermissions: {allow: [customers.read]}\ntemporary_grants: [{permission: invoices.read, starts_at_call: 2, ends_at_call: 3}]\nrevocations: [{permission: customers.read, after_call: 3}]\n")
	r.expect(t, []step{
		{"listInvoices", id("C-104"), false, ReasonNotGranted},
		{"listInvoices", id("C-104"), true, ""},
		{"getCustomer", id("C-104"), true, ""},
		{"listInvoices", id("C-104"), false, ReasonNotGranted},
		{"getCustomer", id("C-104"), false, ReasonRevoked},
	})
}

func TestDecideScopesCustomersThroughRelationships(t *testing.T) {
	r := newSecuredRun(t, "version: 1\nprincipal: {id: support}\npermissions: {allow: [customers.read, invoices.read, charges.read, subscriptions.read, refunds.create, crm.accounts.read, crm.notes.write, jira.issues.read, jira.comments.write, jira.issues.create]}\nresources: {customer_ids: [C-104]}\n")
	r.expect(t, []step{
		{"getCustomer", id("C-104"), true, ""},
		{"getCustomer", id("C-205"), false, ReasonCustomerScope},
		{"listInvoices", id("C-205"), false, ReasonCustomerScope},
		{"listCharges", id("INV-104"), true, ""},
		{"listCharges", id("INV-205"), false, ReasonCustomerScope},
		{"getCharge", id("CH-2001"), false, ReasonCustomerScope},
		{"getCharge", id("CH-9999"), false, ReasonCustomerScope},
		{"getSubscription", id("SUB-104"), true, ""},
		{"getSubscription", id("SUB-205"), false, ReasonCustomerScope},
		{"createRefund", map[string]any{"charge_id": "CH-2001", "amount_cents": 100, "reason": "x"}, false, ReasonCustomerScope},
		{"crmGetAccount", id("A-104"), true, ""},
		{"crmGetAccount", id("A-205"), false, ReasonCustomerScope},
		{"crmAddAccountNote", map[string]any{"account_id": "A-205", "body": "x"}, false, ReasonCustomerScope},
		{"crmSearchAccounts", map[string]any{"query": "C-104"}, false, ReasonBroadSearch},
		{"ticketGetIssue", id("ISS-104"), true, ""},
		{"ticketGetIssue", id("ISS-205"), false, ReasonCustomerScope},
		{"ticketAddComment", map[string]any{"issue_id": "ISS-205", "body": "x"}, false, ReasonCustomerScope},
		{"ticketAddComment", map[string]any{"issue_id": "ISS-104", "body": "x"}, true, ""},
		{"getCharge", id("CH-1001"), true, ""},
		{"createRefund", map[string]any{"charge_id": "CH-1002", "amount_cents": 100, "reason": "x"}, true, ""},
		{"ticketSearchIssues", map[string]any{"query": "charge"}, false, ReasonBroadSearch},
		{"ticketCreateIssue", map[string]any{"account_id": "A-205", "project_id": "PROJ-ENG", "title": "x", "priority": "low"}, false, ReasonCustomerScope},
	})
}

func TestDecideScopesChannelsAndProjects(t *testing.T) {
	empty := newSecuredRun(t, "version: 1\nprincipal: {id: support}\npermissions: {allow: [slack.channels.read, slack.messages.read, slack.messages.write]}\nresources: {channel_ids: []}\n")
	empty.expect(t, []step{
		{"messageReadChannel", id("CH-SUPPORT"), false, ReasonChannelScope},
		{"messagePostMessage", map[string]any{"channel_id": "CH-SUPPORT", "body": "x"}, false, ReasonChannelScope},
		{"messageListChannels", id("WS-1"), false, ReasonBroadSearch},
	})
	scoped := newSecuredRun(t, "version: 1\nprincipal: {id: support}\npermissions: {allow: [slack.messages.write, jira.issues.create]}\nresources: {channel_ids: [CH-SUPPORT], project_ids: [PROJ-OPS]}\n")
	scoped.expect(t, []step{
		{"messagePostMessage", map[string]any{"channel_id": "CH-SUPPORT", "body": "x"}, true, ""},
		{"messagePostMessage", map[string]any{"channel_id": "CH-OTHER", "body": "x"}, false, ReasonChannelScope},
		{"ticketCreateIssue", map[string]any{"account_id": "A-104", "project_id": "PROJ-ENG", "title": "x", "priority": "low"}, false, ReasonProjectScope},
	})
	unscoped := newSecuredRun(t, "version: 1\nprincipal: {id: support}\npermissions: {allow: [slack.channels.read, crm.accounts.read]}\n")
	unscoped.expect(t, []step{
		{"messageListChannels", id("WS-1"), true, ""},
		{"crmSearchAccounts", map[string]any{"query": "C-205"}, true, ""},
	})
}

func TestDecideCapsRefundAmount(t *testing.T) {
	r := newSecuredRun(t, "version: 1\nprincipal: {id: support}\npermissions: {allow: [refunds.create]}\nconstraints: {refund_max_cents: 500}\n")
	refund := func(amount any) map[string]any {
		return map[string]any{"charge_id": "CH-1002", "amount_cents": amount, "reason": "dup"}
	}
	r.expect(t, []step{
		{"createRefund", refund(501), false, ReasonRefundLimit},
		{"createRefund", refund(500), true, ""},
		{"createRefund", refund(float64(600)), false, ReasonRefundLimit},
	})
}

func TestDecideWithoutPolicyIsUnenforced(t *testing.T) {
	r := newSecuredRun(t, "")
	if got := r.decide(t, "createRefund", map[string]any{"charge_id": "CH-2001", "amount_cents": 100000, "reason": "x"}); got.Enforced {
		t.Fatalf("unrestricted run enforced: %+v", got)
	}
	var rows int
	if err := r.store.DB.QueryRow("SELECT count(*) FROM auth_state").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("unrestricted run advanced a counter: %d %v", rows, err)
	}
}

func TestDecideDeniesUnmappedBehavior(t *testing.T) {
	r := newSecuredRun(t, "version: 1\nprincipal: {id: support}\npermissions: {allow: [customers.read]}\n")
	ctx := context.Background()
	tx, err := r.store.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	got, err := Decide(ctx, tx, r.run.ID, r.run.WorldID, compiler.Operation{ID: "custom", Behavior: "custom.export"}, map[string]any{})
	if err != nil || !got.Enforced || got.Allowed || got.Reason != ReasonUnmapped {
		t.Fatalf("decision=%+v err=%v", got, err)
	}
}

func TestDecideRejectsTamperedStoredPolicy(t *testing.T) {
	r := newSecuredRun(t, "version: 1\nprincipal: {id: support}\npermissions: {allow: [customers.read]}\n")
	if _, err := r.store.DB.Exec(`UPDATE run_auth SET policy_json=replace(policy_json,'customers.read','charges.read')`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := r.store.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := Decide(ctx, tx, r.run.ID, r.run.WorldID, *r.manifest.Operation("getCharge"), id("CH-1001")); err == nil {
		t.Fatal("tampered policy accepted")
	}
}

func TestExposedFiltersByNextCallAndScope(t *testing.T) {
	ids := func(ops []compiler.Operation) map[string]bool {
		out := map[string]bool{}
		for _, op := range ops {
			out[op.ID] = true
		}
		return out
	}
	ctx := context.Background()
	r := newSecuredRun(t, "version: 1\nprincipal: {id: support}\npermissions: {allow: [customers.read, crm.accounts.read]}\nresources: {customer_ids: [C-104]}\ntemporary_grants: [{permission: invoices.read, starts_at_call: 2, ends_at_call: 2}]\n")
	first, err := Exposed(ctx, r.store.DB, r.run.ID, r.manifest.Operations)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(first); len(got) != 2 || !got["getCustomer"] || !got["crmGetAccount"] {
		t.Fatalf("first exposure=%v", got)
	}
	r.decide(t, "getCustomer", id("C-104"))
	second, err := Exposed(ctx, r.store.DB, r.run.ID, r.manifest.Operations)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(second); len(got) != 3 || !got["listInvoices"] {
		t.Fatalf("second exposure=%v", got)
	}
	open := newSecuredRun(t, "")
	all, err := Exposed(ctx, open.store.DB, open.run.ID, open.manifest.Operations)
	if err != nil || len(all) != len(open.manifest.Operations) {
		t.Fatalf("unrestricted exposure=%d err=%v", len(all), err)
	}
}
