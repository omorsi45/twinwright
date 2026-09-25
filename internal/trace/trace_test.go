package trace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/authz"
	"twinwright/internal/behavior"
	"twinwright/internal/chaos"
	"twinwright/internal/checkpoint"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/fork"
	"twinwright/internal/store"
)

func billingManifest(t *testing.T) compiler.Manifest {
	t.Helper()
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
	return manifest
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

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "world.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func execute(t *testing.T, s *store.Store, manifest compiler.Manifest, scenario, model string, options store.RunOptions, provider agent.Provider, steps ...int) store.Run {
	t.Helper()
	ctx := context.Background()
	world, err := s.SeedScenario(ctx, 42, manifest.Digest, scenario)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRunConfigured(ctx, world.ID, scenario, "scripted", model, "task", options)
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: provider}
	if len(steps) == 0 {
		steps = []int{20}
	}
	for _, n := range steps {
		if run, err = runner.Execute(ctx, run.ID, n); err != nil {
			t.Fatal(err)
		}
	}
	return run
}

func build(t *testing.T, s *store.Store, runID string) Trace {
	t.Helper()
	trace, err := Build(context.Background(), s, runID)
	if err != nil {
		t.Fatal(err)
	}
	return trace
}

func toolSpans(root *Span) []*Span {
	var spans []*Span
	for _, child := range root.Children {
		if child.Name == "tool.call" {
			spans = append(spans, child)
		}
	}
	return spans
}

func eventNames(span *Span) []string {
	var names []string
	for _, event := range span.Events {
		names = append(names, event.Name)
	}
	return names
}

func TestBuildLegacyFaultRetryAndPause(t *testing.T) {
	s := newStore(t)
	run := execute(t, s, billingManifest(t), "duplicate-charge", "fixture-v1", store.RunOptions{FaultOperation: "listCharges"}, agent.ScriptedProvider{}, 3, 20)
	trace := build(t, s, run.ID)
	root := trace.Root
	if root.Name != "run" || root.Status != "ok" || root.Attributes["run.id"] != run.ID || root.Attributes["scenario"] != "duplicate-charge" || root.Attributes["principal"] != store.UnrestrictedPrincipal {
		t.Fatalf("root=%+v", root)
	}
	if !strings.Contains(strings.Join(eventNames(root), ","), "execution.paused") || !strings.Contains(strings.Join(eventNames(root), ","), "execution.completed") {
		t.Fatalf("root events=%v", eventNames(root))
	}
	tools := toolSpans(root)
	if len(tools) != 5 {
		t.Fatalf("tool spans=%d", len(tools))
	}
	failed := tools[2]
	if failed.Attributes["operation"] != "listCharges" || failed.Attributes["http.status"] != 503 || failed.Attributes["error.type"] != "server_error" || failed.Status != "error" || eventNames(failed)[0] != "fault.injected" {
		t.Fatalf("failed call=%+v", failed)
	}
	if retried := tools[3]; retried.Status != "ok" || !contains(eventNames(retried), "retry") {
		t.Fatalf("retried call=%+v", retried)
	}
	if refund := tools[4]; !contains(eventNames(refund), "state.mutation") || refund.Events[0].Attributes["mutation.charge_id"] != "CH-1002" || refund.Attributes["checkpoint"] != true {
		t.Fatalf("refund call=%+v", refund)
	}
	last := root.Children[len(root.Children)-1]
	if last.Name != "evaluation" || last.Status != "ok" || last.Attributes["computed_at_read_time"] != true {
		t.Fatalf("evaluation span=%+v", last)
	}
	sum := trace.Summary
	if sum.RunID != run.ID || sum.Status != "completed" || sum.ToolCalls != 5 || sum.FailedToolCalls != 1 || sum.Retries != 1 || sum.Faults != 1 || sum.StateMutations != 1 || sum.ModelTurns != run.Step || sum.TokenUsage.Recorded || sum.Evaluation == nil || !sum.Evaluation.Passed {
		t.Fatalf("summary=%+v", sum)
	}
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func TestBuildChaosLostResponse(t *testing.T) {
	s := newStore(t)
	manifest := billingManifest(t)
	policy, err := chaos.Parse([]byte("version: 1\nrules:\n  - id: lost\n    type: timeout_after_commit\n    operations: [createRefund]\n    times: 1\n  - id: slow\n    type: latency\n    operations: [createRefund]\n    duration_ms: 250\n"), manifest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := policy.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	run := execute(t, s, manifest, "ambiguous-commit", "fixture-unsafe-v1", store.RunOptions{ChaosJSON: encoded, ChaosDigest: policy.Digest()}, agent.AmbiguousScriptedProvider{Unsafe: true})
	trace := build(t, s, run.ID)
	tools := toolSpans(trace.Root)
	lost := tools[0]
	if lost.Attributes["error.type"] != "transport_timeout" || lost.Attributes["http.status"] != 0 || !contains(eventNames(lost), "fault.injected") || !contains(eventNames(lost), "state.mutation") {
		t.Fatalf("lost response span=%+v", lost)
	}
	if trace.Summary.SimulatedLatencyMS != 250 || trace.Summary.Evaluation == nil || trace.Summary.Evaluation.Passed || trace.Root.Children[len(trace.Root.Children)-1].Status != "error" {
		t.Fatalf("summary=%+v", trace.Summary)
	}
}

func TestBuildAuthorizationDenial(t *testing.T) {
	s := newStore(t)
	manifest := companyManifest(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "security", "support-policy.yaml"))
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
	run := execute(t, s, manifest, "prompt-injection-ticket", "fixture-v1", store.RunOptions{AuthJSON: encoded, AuthDigest: policy.Digest()}, agent.SecurityScriptedProvider{})
	trace := build(t, s, run.ID)
	denied := toolSpans(trace.Root)[1]
	if denied.Attributes["operation"] != "getCustomer" || denied.Attributes["error.type"] != "authorization_denied" {
		t.Fatalf("denied span=%+v", denied)
	}
	check := denied.Events[0]
	if check.Name != "authorization.check" || check.Attributes["decision"] != "denied" || check.Attributes["reason"] != "customer_out_of_scope" || check.Attributes["principal"] != "support-agent-1" {
		t.Fatalf("authorization event=%+v", check)
	}
	if trace.Root.Attributes["principal"] != "support-agent-1" || trace.Summary.AuthorizationDenied != 2 || trace.Summary.AuthorizationAllowed != 2 {
		t.Fatalf("summary=%+v", trace.Summary)
	}
}

func TestBuildForkWithObservationOverride(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "world.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	manifest := billingManifest(t)
	parent := execute(t, s, manifest, "duplicate-charge", "fixture-v1", store.RunOptions{}, agent.ScriptedProvider{})
	source, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	points, err := checkpoint.List(ctx, source, parent.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	var selected checkpoint.Checkpoint
	for _, point := range points {
		if point.EventType == "tool.response" && point.ToolCalls == 3 {
			selected = point
		}
	}
	created, err := fork.Create(ctx, source, s, selected, manifest, fork.Options{Observation: &store.Observation{CallID: "fixture-3", Status: 200, Body: json.RawMessage(`[{"amount_cents":1,"id":"CH-1002"},{"amount_cents":1,"id":"CH-1001"}]`)}})
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: agent.ScriptedProvider{}}
	if _, err := runner.Execute(ctx, created.Run.ID, 20); err != nil {
		t.Fatal(err)
	}
	trace := build(t, s, created.Run.ID)
	root := trace.Root
	if root.Attributes["parent.run_id"] != parent.ID || root.Attributes["fork.event_seq"] != selected.EventSeq || trace.Summary.ParentRunID != parent.ID {
		t.Fatalf("fork root=%+v", root.Attributes)
	}
	if names := eventNames(root); len(names) < 2 || names[0] != "fork" || names[1] != "observation.overridden" {
		t.Fatalf("fork events=%v", names)
	}
	if root.Events[1].Attributes["call_id"] != "fixture-3" || root.Events[1].Attributes["original_status"] != 200 {
		t.Fatalf("override event=%+v", root.Events[1])
	}
	for _, child := range root.Children {
		if _, ok := child.Attributes["checkpoint"]; ok {
			t.Fatalf("fork span claims a checkpoint: %+v", child)
		}
	}
}

type failingProvider struct{}

func (failingProvider) Next(context.Context, string, []agent.Message, []compiler.Operation) (agent.Message, error) {
	return agent.Message{}, errors.New("upstream rejected Authorization: Bearer abc.def-ghi")
}

func TestBuildFailedProviderTurnIsRedacted(t *testing.T) {
	s := newStore(t)
	manifest := billingManifest(t)
	ctx := context.Background()
	world, err := s.SeedScenario(ctx, 42, manifest.Digest, "duplicate-charge")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: failingProvider{}}
	if _, err := runner.Execute(ctx, run.ID, 5); err == nil {
		t.Fatal("provider failure not reported")
	}
	trace := build(t, s, run.ID)
	model := trace.Root.Children[0]
	if trace.Root.Status != "error" || model.Name != "model.invocation" || model.Status != "error" || model.Attributes["error.type"] != "provider" {
		t.Fatalf("root=%+v model=%+v", trace.Root, model)
	}
	encoded, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "abc.def-ghi") || !strings.Contains(string(encoded), "[REDACTED]") || trace.Summary.Errors != 1 {
		t.Fatalf("trace=%s", encoded)
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	s := newStore(t)
	run := execute(t, s, billingManifest(t), "duplicate-charge", "fixture-v1", store.RunOptions{FaultOperation: "listCharges"}, agent.ScriptedProvider{})
	first, err := json.Marshal(build(t, s, run.ID))
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(build(t, s, run.ID))
	if err != nil {
		t.Fatal(err)
	}
	trace := build(t, s, run.ID)
	if string(first) != string(second) || len(trace.TraceID) != 32 || len(trace.Root.SpanID) != 16 || trace.Root.Children[0].SpanID == trace.Root.SpanID {
		t.Fatalf("trace IDs trace=%s root=%s", trace.TraceID, trace.Root.SpanID)
	}
	if _, err := Build(context.Background(), s, "R-missing"); err == nil {
		t.Fatal("missing run traced")
	}
}
