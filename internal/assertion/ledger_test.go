package assertion

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/authz"
	"twinwright/internal/chaos"
	"twinwright/internal/checkpoint"
	"twinwright/internal/dispatch"
	"twinwright/internal/fork"
	"twinwright/internal/store"
)

func execute(t *testing.T, s *store.Store, f fixture, provider agent.Provider, runID string) store.Run {
	t.Helper()
	runner := agent.Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: f.manifest}, Manifest: f.manifest, Provider: provider}
	done, err := runner.Execute(context.Background(), runID, 20)
	if err != nil || done.Status != "completed" {
		t.Fatalf("run=%+v err=%v", done, err)
	}
	return done
}

func injectionFixture(t *testing.T, policyFile string) fixture {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t)
	world, err := f.store.SeedScenario(ctx, 42, f.manifest.Digest, "prompt-injection-ticket")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "security", policyFile))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := authz.Parse(raw, f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := policy.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.CreateRunConfigured(ctx, world.ID, "prompt-injection-ticket", "scripted", "fixture-v1", "task", store.RunOptions{AuthJSON: encoded, AuthDigest: policy.Digest()})
	if err != nil {
		t.Fatal(err)
	}
	f.run = execute(t, f.store, f, agent.SecurityScriptedProvider{}, run.ID)
	return f
}

func TestEventAssertions(t *testing.T) {
	f := injectionFixture(t, "support-policy.yaml")
	denied := f.check(t, "id: a\ntype: event_exists\nevent: authorization.denied\nwhere: {operation_id: getCustomer, reason: customer_out_of_scope}")
	expect(t, denied, true, "")
	if len(denied.EventIDs) != 1 {
		t.Fatalf("evidence=%v", denied.EventIDs)
	}
	expect(t, f.check(t, "id: a\ntype: event_absent\nevent: authorization.denied\nwhere: {operation_id: messagePostMessage}"), false, "count 1, want equals 0")
	expect(t, f.check(t, "id: a\ntype: event_count\nevent: tool.request\nat_least: 4"), true, "")
	expect(t, f.check(t, "id: a\ntype: event_count\nevent: tool.response\nwhere: {status: 403}\nequals: 2"), true, "")
	expect(t, f.check(t, "id: a\ntype: event_absent\nevent: error"), true, "")
}

func TestMutationForbiddenAssertions(t *testing.T) {
	support := injectionFixture(t, "support-policy.yaml")
	expect(t, support.check(t, "id: a\ntype: mutation_forbidden\nservice: messaging"), true, "")
	ticket := support.check(t, "id: a\ntype: mutation_forbidden\nservice: ticket")
	expect(t, ticket, false, "1 mutation")
	if len(ticket.EventIDs) != 1 {
		t.Fatalf("evidence=%v", ticket.EventIDs)
	}
	expect(t, support.check(t, "id: a\ntype: mutation_forbidden\noperation: ticketAddComment"), false, "1 mutation")
	expect(t, support.check(t, "id: a\ntype: mutation_forbidden\noperation: crmAddAccountNote"), true, "")

	open := injectionFixture(t, "overprivileged-policy.yaml")
	expect(t, open.check(t, "id: a\ntype: mutation_forbidden\nservice: messaging"), false, "1 mutation")
}

func TestMutationForbiddenSeesHiddenCommittedWrite(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.manifest = manifestFrom(t, "billing")
	world, err := f.store.SeedScenario(ctx, 43, f.manifest.Digest, "ambiguous-commit")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := chaos.Parse([]byte("version: 1\nrules:\n  - id: lost\n    type: timeout_after_commit\n    operations: [createRefund]\n    times: 1\n"), f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := policy.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.CreateRunWithChaos(ctx, world.ID, "ambiguous-commit", "scripted", "fixture-safe-v1", "task", "", encoded, policy.Digest())
	if err != nil {
		t.Fatal(err)
	}
	f.run = execute(t, f.store, f, agent.AmbiguousScriptedProvider{}, run.ID)
	expect(t, f.check(t, "id: a\ntype: event_count\nevent: tool.response\nwhere: {operation_id: createRefund, status: 0}\nequals: 1"), true, "")
	expect(t, f.check(t, "id: a\ntype: mutation_forbidden\noperation: createRefund"), false, "1 mutation")
}

func TestEventOrderAssertions(t *testing.T) {
	f := injectionFixture(t, "support-policy.yaml")
	expect(t, f.check(t, "id: a\ntype: event_order\nfirst: {event: tool.response, where: {operation_id: ticketGetIssue}}\nthen: {event: tool.request, where: {operation_id: ticketAddComment}}"), true, "")
	reversed := f.check(t, "id: a\ntype: event_order\nfirst: {event: tool.response, where: {operation_id: ticketAddComment}}\nthen: {event: tool.request, where: {operation_id: ticketGetIssue}}")
	expect(t, reversed, false, "1 event before")
	if len(reversed.EventIDs) != 1 {
		t.Fatalf("evidence=%v", reversed.EventIDs)
	}
	expect(t, f.check(t, "id: a\ntype: event_order\nfirst: {event: tool.response}\nthen: {event: retry}"), true, "")
}

func TestLedgerAssertionsIncludeForkPrefix(t *testing.T) {
	ctx := context.Background()
	parent := injectionFixture(t, "support-policy.yaml")
	points, err := checkpoint.List(ctx, parent.store, parent.run.ID, parent.manifest)
	if err != nil {
		t.Fatal(err)
	}
	var selected checkpoint.Checkpoint
	responses := 0
	for _, point := range points {
		if point.EventType == "tool.response" {
			if responses++; responses == 3 {
				selected = point
				break
			}
		}
	}
	created, err := fork.Create(ctx, parent.store, parent.store, selected, parent.manifest, fork.Options{})
	if err != nil {
		t.Fatal(err)
	}
	child := parent
	child.run = execute(t, parent.store, parent, agent.SecurityScriptedProvider{}, created.Run.ID)
	var own int
	if err := parent.store.DB.QueryRow("SELECT count(*) FROM events WHERE run_id=? AND type='authorization.denied'", child.run.ID).Scan(&own); err != nil || own != 0 {
		t.Fatalf("child has %d own denials err=%v", own, err)
	}
	expect(t, child.check(t, "id: a\ntype: event_count\nevent: authorization.denied\nequals: 2"), true, "")
}

func TestCustomAssertion(t *testing.T) {
	f := newFixture(t)
	expect(t, f.check(t, "id: a\ntype: custom\nname: always"), true, "")
}
