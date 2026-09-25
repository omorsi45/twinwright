package replay

import (
	"context"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/authz"
	"twinwright/internal/checkpoint"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/fork"
	"twinwright/internal/store"
)

const securedPolicy = "version: 1\nprincipal: {id: support}\npermissions: {allow: [customers.read, invoices.read, charges.read]}\nresources: {customer_ids: [C-104]}\ntemporary_grants: [{permission: refunds.create, starts_at_call: 5, ends_at_call: 5}]\n"

// probeProvider first requests an out-of-scope customer, then follows the
// billing fixture as if the probe had not happened.
type probeProvider struct{}

func (probeProvider) Next(ctx context.Context, task string, history []agent.Message, ops []compiler.Operation) (agent.Message, error) {
	var rest []agent.Message
	probed := false
	for _, m := range history {
		if m.CallID == "probe" {
			probed = true
			continue
		}
		if m.Role == "tool" {
			rest = append(rest, m)
		}
	}
	if !probed {
		return agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: "probe", OperationID: "getCustomer", Arguments: map[string]any{"id": "C-205"}}}}, nil
	}
	return agent.ScriptedProvider{}.Next(ctx, task, rest, ops)
}

func runToCompletion(t *testing.T, s *store.Store, manifest compiler.Manifest, runID string) store.Run {
	t.Helper()
	runner := agent.Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: probeProvider{}}
	done, err := runner.Execute(context.Background(), runID, 20)
	if err != nil || done.Status != "completed" {
		t.Fatalf("run=%+v err=%v", done, err)
	}
	return done
}

func completedSecuredRun(t *testing.T) (*store.Store, store.Run, compiler.Manifest) {
	t.Helper()
	ctx := context.Background()
	source, _, manifest := completedRun(t)
	world, err := source.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := authz.Parse([]byte(securedPolicy), manifest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := policy.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	run, err := source.CreateRunConfigured(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "task", store.RunOptions{AuthJSON: encoded, AuthDigest: policy.Digest()})
	if err != nil {
		t.Fatal(err)
	}
	return source, runToCompletion(t, source, manifest, run.ID), manifest
}

func firstCheckpoint(t *testing.T, s *store.Store, runID string, manifest compiler.Manifest, eventType string) checkpoint.Checkpoint {
	t.Helper()
	points, err := checkpoint.List(context.Background(), s, runID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range points {
		if point.EventType == eventType {
			return point
		}
	}
	t.Fatalf("no %s checkpoint", eventType)
	return checkpoint.Checkpoint{}
}

func TestVerifySecuredRunAndInheritedForks(t *testing.T) {
	ctx := context.Background()
	source, parent, manifest := completedSecuredRun(t)
	var denied int
	if err := source.DB.QueryRow("SELECT count(*) FROM events WHERE run_id=? AND type='authorization.denied'", parent.ID).Scan(&denied); err != nil || denied != 1 {
		t.Fatalf("denied events=%d err=%v", denied, err)
	}
	if report, err := Verify(ctx, source, parent.ID, manifest); err != nil || !report.Verified {
		t.Fatalf("root replay=%+v err=%v", report, err)
	}
	for _, eventType := range []string{"model.response", "tool.response"} {
		t.Run(eventType, func(t *testing.T) {
			created, err := fork.Create(ctx, source, source, firstCheckpoint(t, source, parent.ID, manifest, eventType), manifest, fork.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if created.Run.PrincipalID != "support" {
				t.Fatalf("child principal=%q", created.Run.PrincipalID)
			}
			child := runToCompletion(t, source, manifest, created.Run.ID)
			var refunds int
			if err := source.DB.QueryRow("SELECT count(*) FROM refunds WHERE world_id=?", child.WorldID).Scan(&refunds); err != nil || refunds != 1 {
				t.Fatalf("child refunds=%d err=%v; call counter not inherited", refunds, err)
			}
			if report, err := Verify(ctx, source, child.ID, manifest); err != nil || !report.Verified {
				t.Fatalf("child replay=%+v err=%v", report, err)
			}
		})
	}
}

func TestVerifyForkWithReplacementAuthorization(t *testing.T) {
	ctx := context.Background()
	source, parent, manifest := completedSecuredRun(t)
	replacement := []byte("version: 1\nprincipal: {id: auditor}\npermissions: {allow: [customers.read, invoices.read, charges.read, refunds.create]}\n")
	created, err := fork.Create(ctx, source, source, firstCheckpoint(t, source, parent.ID, manifest, "model.response"), manifest, fork.Options{AuthPolicyRaw: replacement})
	if err != nil {
		t.Fatal(err)
	}
	lineage, err := source.Lineage(ctx, created.Run.ID)
	if err != nil || !lineage.AuthReplaced {
		t.Fatalf("lineage=%+v err=%v", lineage, err)
	}
	if created.Run.PrincipalID != "auditor" {
		t.Fatalf("child principal=%q", created.Run.PrincipalID)
	}
	var calls int
	if err := source.DB.QueryRow("SELECT count(*) FROM auth_state WHERE run_id=?", created.Run.ID).Scan(&calls); err != nil || calls != 0 {
		t.Fatalf("replacement inherited call counter: rows=%d err=%v", calls, err)
	}
	child := runToCompletion(t, source, manifest, created.Run.ID)
	var probe int
	if err := source.DB.QueryRow("SELECT status FROM tool_results WHERE run_id=? AND call_id='probe'", child.ID).Scan(&probe); err != nil || probe != 200 {
		t.Fatalf("replacement probe status=%d err=%v", probe, err)
	}
	if report, err := Verify(ctx, source, child.ID, manifest); err != nil || !report.Verified {
		t.Fatalf("replacement replay=%+v err=%v", report, err)
	}
	if err := fork.ValidateOptions(parent, manifest, fork.Options{AuthPolicyRaw: []byte("version: 1\nprincipal: {id: ''}")}); err == nil {
		t.Fatal("invalid replacement policy accepted")
	}
}

func TestVerifySecuredRunRejectsTampering(t *testing.T) {
	for _, update := range []string{
		"UPDATE run_auth SET policy_json=replace(policy_json,'C-104','C-205') WHERE run_id=?",
		"UPDATE events SET payload=replace(payload,'customer_out_of_scope','permission_not_granted') WHERE run_id=? AND type='authorization.denied'",
		"UPDATE auth_state SET call_index=99 WHERE run_id=?",
		"UPDATE runs SET principal_id='someone-else' WHERE id=?",
		"UPDATE tool_results SET status=200 WHERE run_id=? AND call_id='probe'",
	} {
		t.Run(update, func(t *testing.T) {
			source, run, manifest := completedSecuredRun(t)
			if _, err := source.DB.ExecContext(context.Background(), update, run.ID); err != nil {
				t.Fatal(err)
			}
			if report, err := Verify(context.Background(), source, run.ID, manifest); err == nil && report.Verified {
				t.Fatal("tampered authorization history accepted")
			}
		})
	}
}
