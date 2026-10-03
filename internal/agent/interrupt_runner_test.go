package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

// interruptThen fails its first n attempts with the given interruption and then
// behaves like the scenario fixture.
type interruptThen struct {
	remaining *int
	interrupt *CallInterrupted
}

func (p interruptThen) Next(ctx context.Context, task string, history []Message, ops []compiler.Operation) (Message, error) {
	if *p.remaining > 0 {
		*p.remaining--
		return Message{RawBody: "partial"}, p.interrupt
	}
	return scenarioProvider{}.Next(ctx, task, history, ops)
}

func interruptRun(t *testing.T) (*store.Store, Runner, store.Run) {
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
	world, err := s.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "test", "test-model", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest}
	return s, runner, run
}

func interruptionsIn(t *testing.T, s *store.Store, runID string) []Interruption {
	t.Helper()
	events, err := s.Events(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []Interruption
	for _, event := range events {
		if event.Type != "provider.interrupted" {
			continue
		}
		var recorded Interruption
		if err := json.Unmarshal(event.Payload, &recorded); err != nil {
			t.Fatal(err)
		}
		out = append(out, recorded)
	}
	return out
}

// A provider that hung for one attempt is the common case, and killing a long run
// over it is the wrong answer. One retry, and the attempt that failed goes in the
// ledger: a silent retry would make a run that took two calls look like one that
// took one.
func TestRunnerRetriesAnInterruptedAttemptOnceAndRecordsIt(t *testing.T) {
	ctx := context.Background()
	s, runner, run := interruptRun(t)
	remaining := 1
	runner.Provider = interruptThen{remaining: &remaining, interrupt: &CallInterrupted{Kind: InterruptTimeout, Deadline: 250 * time.Millisecond}}

	completed, err := runner.Execute(ctx, run.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "completed" {
		t.Fatalf("status=%s", completed.Status)
	}
	recorded := interruptionsIn(t, s, run.ID)
	if len(recorded) != 1 {
		t.Fatalf("want one recorded interruption, got %+v", recorded)
	}
	if recorded[0].Kind != InterruptTimeout || recorded[0].Attempt != 1 || recorded[0].DeadlineMS != 250 {
		t.Fatalf("%+v", recorded[0])
	}

	// The ledger has to stay balanced: one request, one response per turn. A
	// second request for the retry would make replay reject the run forever.
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	requests, responses := 0, 0
	for _, event := range events {
		switch event.Type {
		case "model.request":
			requests++
		case "model.response":
			responses++
		}
	}
	if requests != responses {
		t.Fatalf("ledger has %d model requests and %d responses", requests, responses)
	}

	// The turn that was retried carries the interruption, so a replay of this run
	// reproduces the same ledger from the record instead of needing the provider
	// to hang again.
	var history []Message
	if err := json.Unmarshal([]byte(completed.Transcript), &history); err != nil {
		t.Fatal(err)
	}
	carried := 0
	for _, m := range history {
		carried += len(m.Interruptions)
	}
	if carried != 1 {
		t.Fatalf("the recorded turn does not carry the interruption: %s", completed.Transcript)
	}
}

// A truncated completion that stays truncated is not something a retry can fix,
// and consuming it would mean acting on a fragment. The run fails, with both
// attempts in the ledger and no tool call executed.
func TestRunnerFailsWhenEveryAttemptIsInterrupted(t *testing.T) {
	ctx := context.Background()
	s, runner, run := interruptRun(t)
	always := 99
	runner.Provider = interruptThen{remaining: &always, interrupt: &CallInterrupted{Kind: InterruptTruncated, Reason: "length"}}

	if _, err := runner.Execute(ctx, run.ID, 10); err == nil {
		t.Fatal("a run whose every attempt was interrupted must fail")
	}
	failed, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" {
		t.Fatalf("status=%s", failed.Status)
	}
	recorded := interruptionsIn(t, s, run.ID)
	if len(recorded) != 2 {
		t.Fatalf("both attempts must be recorded, got %+v", recorded)
	}
	if recorded[0].Attempt != 1 || recorded[1].Attempt != 2 || recorded[1].Reason != "length" {
		t.Fatalf("%+v", recorded)
	}
	var results int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM tool_results WHERE run_id=?", run.ID).Scan(&results); err != nil {
		t.Fatal(err)
	}
	if results != 0 {
		t.Fatalf("a truncated completion produced %d tool calls", results)
	}
}
