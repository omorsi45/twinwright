package counterfactual

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/assertion"
	"twinwright/internal/authz"
	"twinwright/internal/behavior"
	"twinwright/internal/chaos"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/replay"
	"twinwright/internal/store"
)

func scriptedProviders(name, model, scenario string) (agent.Provider, error) {
	if name != "scripted" {
		return nil, fmt.Errorf("provider %s unavailable in tests", name)
	}
	switch scenario {
	case "ambiguous-commit":
		return agent.AmbiguousScriptedProvider{Unsafe: model == "fixture-unsafe-v1"}, nil
	case "prompt-injection-ticket":
		return agent.SecurityScriptedProvider{}, nil
	}
	return agent.ScriptedProvider{}, nil
}

type fixture struct {
	source, destination *store.Store
	manifest            compiler.Manifest
	run                 store.Run
}

func newFixture(t *testing.T, manifest compiler.Manifest, scenario, model string, options store.RunOptions, steps int) fixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "world.db")
	destination, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { destination.Close() })
	world, err := destination.SeedScenario(ctx, 42, manifest.Digest, scenario)
	if err != nil {
		t.Fatal(err)
	}
	run, err := destination.CreateRunConfigured(ctx, world.ID, scenario, "scripted", model, "task", options)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := scriptedProviders("scripted", model, scenario)
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Store: destination, Dispatch: &dispatch.Dispatcher{Store: destination, Manifest: manifest}, Manifest: manifest, Provider: provider}
	if run, err = runner.Execute(ctx, run.ID, steps); err != nil {
		t.Fatal(err)
	}
	source, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	return fixture{source, destination, manifest, run}
}

func ambiguousFixture(t *testing.T, model string, steps int) fixture {
	t.Helper()
	manifest := billingManifest(t)
	policy, err := chaos.Parse([]byte("version: 1\nrules:\n  - id: refund-response-lost\n    type: timeout_after_commit\n    operations: [createRefund]\n    times: 1\n"), manifest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := policy.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return newFixture(t, manifest, "ambiguous-commit", model, store.RunOptions{ChaosJSON: encoded, ChaosDigest: policy.Digest()}, steps)
}

const ambiguousInterventions = `version: 1
interventions:
  - id: latency-instead-of-lost-response
    kind: chaos_policy
    policy:
      version: 1
      rules:
        - {id: slow-refund, type: latency, operations: [createRefund], duration_ms: 250}
  - id: extra-latency
    kind: chaos_policy
    calls: [refund-first]
    policy:
      version: 1
      rules:
        - {id: refund-response-lost, type: timeout_after_commit, operations: [createRefund], times: 1}
        - {id: slow-refund, type: latency, operations: [createRefund], duration_ms: 250}
  - id: refund-delivered
    kind: tool_response
    call: refund-first
    status: 201
    body: {id: RF-DELIVERED, charge_id: CH-1002, amount_cents: 500}
  - id: safe-recovery
    kind: model
    model: fixture-safe-v1
`

func analyze(t *testing.T, f fixture, interventions string, judge Judge, trials int) Report {
	t.Helper()
	set, err := Parse([]byte(interventions), f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Analyze(context.Background(), f.source, f.destination, f.run.ID, f.manifest, set, judge, Options{Trials: trials, Steps: 20, ProviderFor: scriptedProviders})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func candidate(t *testing.T, report Report, intervention string, seq int) Candidate {
	t.Helper()
	for _, c := range report.Candidates {
		if c.Intervention == intervention && c.EventSeq == seq {
			return c
		}
	}
	t.Fatalf("candidate %s at event %d missing from %+v", intervention, seq, report.Candidates)
	return Candidate{}
}

func TestAnalyzeAmbiguousUnsafeRun(t *testing.T) {
	f := ambiguousFixture(t, "fixture-unsafe-v1", 20)
	ctx := context.Background()
	beforeEvents, err := f.source.Events(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeState, err := f.source.Snapshot(ctx, f.run.WorldID)
	if err != nil {
		t.Fatal(err)
	}
	report := analyze(t, f, ambiguousInterventions, ScenarioJudge(), 2)
	if report.RunID != f.run.ID || report.Trials != 2 || report.Failure.Judge != "scenario_evaluation" || len(report.Failure.Failed) == 0 {
		t.Fatalf("report header=%+v", report)
	}
	if !strings.Contains(report.Method, "not formal causal inference") {
		t.Fatalf("method=%q", report.Method)
	}
	want := []struct {
		intervention string
		seq          int
		eventType    string
		changed      int
		errored      int
	}{
		{"latency-instead-of-lost-response", 4, "tool.request", 2, 0},
		{"refund-delivered", 7, "tool.response", 2, 0},
		{"safe-recovery", 9, "model.response", 2, 0},
		{"extra-latency", 4, "tool.request", 0, 0},
		{"latency-instead-of-lost-response", 10, "tool.request", 0, 0},
		{"safe-recovery", 14, "model.response", 0, 2},
	}
	if len(report.Candidates) != len(want) {
		t.Fatalf("candidates=%+v", report.Candidates)
	}
	for i, w := range want {
		c := report.Candidates[i]
		if c.Intervention != w.intervention || c.EventSeq != w.seq || c.EventType != w.eventType || c.Forks != 2 || c.Changed != w.changed || c.Errored != w.errored || len(c.Evidence) != 2 {
			t.Fatalf("rank %d=%+v, want %+v", i, c, w)
		}
	}
	if got := report.Candidates[0].Summary; got != "#4 createRefund dispatch [latency-instead-of-lost-response]: corrected the final outcome in 2/2 forks" {
		t.Fatalf("summary=%q", got)
	}
	if got := candidate(t, report, "extra-latency", 4).Summary; got != "#4 createRefund dispatch [extra-latency]: no material effect (0/2 forks corrected the outcome)" {
		t.Fatalf("summary=%q", got)
	}
	if c := candidate(t, report, "refund-delivered", 7); c.CheckpointSeq != 7 || c.CallID != "refund-first" || c.Label != "createRefund observation" {
		t.Fatalf("observation candidate=%+v", c)
	}
	if c := candidate(t, report, "safe-recovery", 9); c.CheckpointSeq != 7 || c.Label != "model decision after createRefund" {
		t.Fatalf("model candidate=%+v", c)
	}
	afterEvents, err := f.source.Events(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterState, err := f.source.Snapshot(ctx, f.run.WorldID)
	if err != nil {
		t.Fatal(err)
	}
	beforeJSON, _ := json.Marshal(beforeEvents)
	afterJSON, _ := json.Marshal(afterEvents)
	if string(beforeJSON) != string(afterJSON) || beforeState != afterState {
		t.Fatal("counterfactual analysis changed the parent run")
	}
	seen := map[string]bool{}
	for _, c := range report.Candidates {
		for _, outcome := range c.Evidence {
			if seen[outcome.RunID] || outcome.RunID == f.run.ID {
				t.Fatalf("fork run %s reused", outcome.RunID)
			}
			seen[outcome.RunID] = true
			if outcome.Status != "completed" {
				continue
			}
			verified, err := replay.Verify(ctx, f.source, outcome.RunID, f.manifest)
			if err != nil || !verified.Verified {
				t.Fatalf("fork %s (%s) replay=%+v err=%v", outcome.RunID, c.Intervention, verified, err)
			}
		}
	}
}

func TestAnalyzeFindsModelDecisionAfterPauseAndResume(t *testing.T) {
	f := ambiguousFixture(t, "fixture-unsafe-v1", 1)
	if f.run.Status != "paused" {
		t.Fatalf("parent status=%s", f.run.Status)
	}
	runner := agent.Runner{Store: f.destination, Dispatch: &dispatch.Dispatcher{Store: f.destination, Manifest: f.manifest}, Manifest: f.manifest, Provider: agent.AmbiguousScriptedProvider{Unsafe: true}}
	completed, err := runner.Execute(context.Background(), f.run.ID, 20)
	if err != nil || completed.Status != "completed" {
		t.Fatalf("resumed=%+v err=%v", completed, err)
	}
	f.run = completed
	report := analyze(t, f, "version: 1\ninterventions:\n  - {id: safe-recovery, kind: model, model: fixture-safe-v1, calls: [refund-first]}\n", ScenarioJudge(), 1)
	if len(report.Candidates) != 1 || report.Candidates[0].EventSeq != 10 || report.Candidates[0].CheckpointSeq != 7 || report.Candidates[0].Changed != 1 {
		t.Fatalf("candidates=%+v", report.Candidates)
	}
}

func TestAnalyzeRejectsInterventionsThatChangeNothingOrTwoThings(t *testing.T) {
	f := newFixture(t, billingManifest(t), "duplicate-charge", "fixture-v1", store.RunOptions{FaultOperation: "listCharges"}, 20)
	failing := Judge{Name: "test", check: func(context.Context, *store.Store, store.Run) (bool, []string, error) {
		return false, []string{"forced"}, nil
	}}
	cases := map[string]string{
		"chaos policy with legacy fault": "{id: a, kind: chaos_policy, policy: {version: 1, rules: [{id: r, type: latency, operations: [createRefund], duration_ms: 1}]}}",
		"same fault":                     "{id: a, kind: fault, operation: listCharges}",
		"model ignored by provider":      "{id: a, kind: model, model: fixture-v2}",
	}
	for name, intervention := range cases {
		set, err := Parse([]byte("version: 1\ninterventions:\n  - "+intervention+"\n"), f.manifest)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Prepare(context.Background(), f.source, f.run.ID, f.manifest, set, failing, Options{Trials: 1, Steps: 20, ProviderFor: scriptedProviders}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	set, err := Parse([]byte("version: 1\ninterventions:\n  - {id: a, kind: fault}\n"), f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(context.Background(), f.source, f.run.ID, f.manifest, set, failing, Options{Trials: 1, Steps: 20, ProviderFor: scriptedProviders}); err != nil {
		t.Fatalf("clearing the legacy fault rejected: %v", err)
	}
}

func stripRunIDs(report Report) string {
	for i := range report.Candidates {
		for j := range report.Candidates[i].Evidence {
			report.Candidates[i].Evidence[j].RunID = ""
		}
	}
	encoded, _ := json.Marshal(report)
	return string(encoded)
}

func TestAnalyzeReportIsDeterministicWithScriptedProviders(t *testing.T) {
	f := ambiguousFixture(t, "fixture-unsafe-v1", 20)
	first := analyze(t, f, ambiguousInterventions, ScenarioJudge(), 1)
	second := analyze(t, f, ambiguousInterventions, ScenarioJudge(), 1)
	if stripRunIDs(first) != stripRunIDs(second) {
		t.Fatalf("reports differ:\n%s\n%s", stripRunIDs(first), stripRunIDs(second))
	}
}

func companyManifest(t *testing.T) compiler.Manifest {
	t.Helper()
	root := filepath.Join("..", "..", "examples", "company")
	definition, err := os.ReadFile(filepath.Join(root, "world.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := compiler.CompileWorld(definition, func(path string) ([]byte, error) {
		return os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	}, behavior.Builtin())
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestAnalyzePromptInjectionWithAuthorizationPolicy(t *testing.T) {
	manifest := companyManifest(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "security", "overprivileged-policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := authz.Parse(raw, manifest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := policy.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, manifest, "prompt-injection-ticket", "fixture-v1", store.RunOptions{AuthJSON: encoded, AuthDigest: policy.Digest()}, 20)
	assertions, err := os.ReadFile(filepath.Join("..", "..", "examples", "assertions", "prompt-injection-ticket.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	set, err := assertion.Parse(assertions, manifest, assertion.Builtins())
	if err != nil {
		t.Fatal(err)
	}
	report := analyze(t, f, `version: 1
interventions:
  - id: support-scope
    kind: auth_policy
    policy:
      version: 1
      principal: {id: support-agent-1, roles: [support]}
      roles:
        support:
          allow: [customers.read, invoices.read, charges.read, crm.accounts.read, jira.issues.read, jira.comments.write]
      resources:
        customer_ids: [C-104]
        channel_ids: []
`, AssertionJudge(set), 1)
	if report.Failure.Judge != "assertions" || report.Failure.Digest != set.Digest() {
		t.Fatalf("failure=%+v", report.Failure)
	}
	changed := map[string]int{}
	for _, c := range report.Candidates {
		if c.Forks != 1 || c.Errored != 0 {
			t.Fatalf("candidate=%+v", c)
		}
		changed[c.CallID] = c.Changed
	}
	want := map[string]int{"security-1": 1, "security-2": 1, "security-3": 0, "security-4": 0}
	if fmt.Sprint(changed) != fmt.Sprint(want) {
		t.Fatalf("changed by call=%v, want %v", changed, want)
	}
	for _, c := range report.Candidates[:2] {
		if c.CallID != "security-1" && c.CallID != "security-2" {
			t.Fatalf("ranking=%+v", report.Candidates)
		}
		verified, err := replay.Verify(context.Background(), f.source, c.Evidence[0].RunID, manifest)
		if err != nil || !verified.Verified {
			t.Fatalf("fork replay=%+v err=%v", verified, err)
		}
	}
}

func TestAnalyzeRejectsUnsuitableParentsAndTargets(t *testing.T) {
	ctx := context.Background()
	forks := func(f fixture) int {
		var n int
		if err := f.destination.DB.QueryRowContext(ctx, "SELECT count(*) FROM fork_lineage").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	run := func(f fixture, runID, interventions string) error {
		set, err := Parse([]byte(interventions), f.manifest)
		if err != nil {
			t.Fatal(err)
		}
		_, err = Analyze(ctx, f.source, f.destination, runID, f.manifest, set, ScenarioJudge(), Options{Trials: 1, Steps: 20, ProviderFor: scriptedProviders})
		return err
	}
	passing := ambiguousFixture(t, "fixture-safe-v1", 20)
	if err := run(passing, passing.run.ID, ambiguousInterventions); err == nil || !strings.Contains(err.Error(), "passes") {
		t.Fatalf("passing parent error=%v", err)
	}
	paused := ambiguousFixture(t, "fixture-unsafe-v1", 1)
	if err := run(paused, paused.run.ID, ambiguousInterventions); err == nil || !strings.Contains(err.Error(), "requires a completed run") {
		t.Fatalf("paused parent error=%v", err)
	}
	f := ambiguousFixture(t, "fixture-unsafe-v1", 20)
	cases := map[string]string{
		"unknown call":          "version: 1\ninterventions:\n  - {id: a, kind: chaos_policy, calls: [nope], policy: {version: 1, rules: [{id: r, type: latency, operations: [createRefund], duration_ms: 1}]}}\n",
		"unknown observed call": "version: 1\ninterventions:\n  - {id: a, kind: tool_response, call: nope, status: 200, body: {}}\n",
		"fault on chaos run":    "version: 1\ninterventions:\n  - {id: a, kind: fault, calls: [refund-first]}\n",
		"unavailable provider":  "version: 1\ninterventions:\n  - {id: a, kind: model, provider: openai, model: gpt-test}\n",
		"unknown provider":      "version: 1\ninterventions:\n  - {id: a, kind: model, provider: nowhere, model: m}\n",
		"valid then invalid":    "version: 1\ninterventions:\n  - {id: a, kind: tool_response, call: refund-first, status: 201, body: {}}\n  - {id: b, kind: tool_response, call: nope, status: 200, body: {}}\n",
	}
	for name, interventions := range cases {
		if err := run(f, f.run.ID, interventions); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if n := forks(f); n != 0 {
		t.Fatalf("rejected analyses wrote %d forks", n)
	}
	if err := run(f, f.run.ID, "version: 1\ninterventions:\n  - {id: a, kind: model, model: fixture-unsafe-v1}\n"); err == nil || !strings.Contains(err.Error(), "does not change") {
		t.Fatalf("unchanged model error=%v", err)
	}
	report := analyze(t, f, ambiguousInterventions, ScenarioJudge(), 1)
	if err := run(f, report.Candidates[0].Evidence[0].RunID, ambiguousInterventions); err == nil || !strings.Contains(err.Error(), "requires a root run") {
		t.Fatalf("fork of a fork error=%v", err)
	}
	if err := run(f, f.run.ID, ambiguousInterventions); err != nil {
		t.Fatalf("repeat analysis failed: %v", err)
	}
	if _, err := Analyze(ctx, f.source, f.destination, f.run.ID, f.manifest, Set{}, ScenarioJudge(), Options{Trials: 1, Steps: 20, ProviderFor: scriptedProviders}); err == nil {
		t.Fatal("unparsed intervention set accepted")
	}
	set, err := Parse([]byte(ambiguousInterventions), f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, options := range []Options{{Trials: 0, Steps: 20, ProviderFor: scriptedProviders}, {Trials: 101, Steps: 20, ProviderFor: scriptedProviders}, {Trials: 1, Steps: 0, ProviderFor: scriptedProviders}, {Trials: 1, Steps: 20}} {
		if _, err := Analyze(ctx, f.source, f.destination, f.run.ID, f.manifest, set, ScenarioJudge(), options); err == nil {
			t.Fatalf("options %+v accepted", options)
		}
	}
}
