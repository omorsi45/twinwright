package agent

import (
	"context"
	"path/filepath"
	"testing"

	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/eval"
	"twinwright/internal/store"
)

// The regressed provider is the gate's proof that it can fail: it must behave
// exactly like the healthy fixture until the first write, and then claim success
// without making it.
func TestRegressedProviderStopsBeforeTheFirstWrite(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s, err := store.Open(filepath.Join(t.TempDir(), "world.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	world, err := s.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-regressed-v1", "task", "listCharges")
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{
		Store:    s,
		Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest},
		Manifest: manifest,
		Provider: RegressedProvider{Inner: ScriptedProvider{}, Manifest: manifest},
	}
	done, err := runner.Execute(ctx, run.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "completed" {
		t.Fatalf("status=%s, want completed: the regression is a wrong answer, not a crash", done.Status)
	}
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type == "state.mutation" {
			t.Fatalf("a mutation was committed: %+v", e)
		}
	}
	report, err := eval.DuplicateCharge(ctx, s, world.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed {
		t.Fatalf("the duplicate-charge evaluation passed without a refund: %+v", report)
	}
}

func TestRegressedProviderPassesReadsAndStopsAtTheFirstWrite(t *testing.T) {
	manifest := billingManifest(t)
	read := Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "r", OperationID: "getCustomer"}}}
	write := Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "w", OperationID: "createRefund"}}}
	for _, tc := range []struct {
		name      string
		inner     Message
		wantCalls int
		wantText  string
	}{
		{"a read is unchanged", read, 1, ""},
		{"the first write becomes a claim of success", write, 0, "The issue is resolved."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := RegressedProvider{Inner: fixedProvider{tc.inner}, Manifest: manifest}
			got, err := p.Next(context.Background(), "task", nil, manifest.Operations)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.ToolCalls) != tc.wantCalls || got.Content != tc.wantText || got.Role != "assistant" {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

type fixedProvider struct{ message Message }

func (f fixedProvider) Next(context.Context, string, []Message, []compiler.Operation) (Message, error) {
	return f.message, nil
}
