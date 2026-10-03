package replay

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"twinwright/internal/agent"
	"twinwright/internal/checkpoint"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/fork"
	"twinwright/internal/store"
)

// interruptOnceProvider hangs past its deadline on the first attempt of the first
// turn and then behaves like the shipped fixture.
type interruptOnceProvider struct{ used bool }

func (p *interruptOnceProvider) Next(ctx context.Context, task string, history []agent.Message, ops []compiler.Operation) (agent.Message, error) {
	if !p.used {
		p.used = true
		return agent.Message{}, &agent.CallInterrupted{Kind: agent.InterruptTimeout, Deadline: 250 * time.Millisecond}
	}
	return agent.ScriptedProvider{}.Next(ctx, task, history, ops)
}

// interruptedRun is a completed run whose first model turn needed two attempts.
func interruptedRun(t *testing.T) (*store.Store, store.Run, compiler.Manifest) {
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
	source, err := store.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	world, err := source.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := source.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "Customer C-104 was charged twice. Refund only the duplicate.", "")
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Store: source, Dispatch: &dispatch.Dispatcher{Store: source, Manifest: manifest}, Manifest: manifest, Provider: &interruptOnceProvider{}}
	done, err := runner.Execute(ctx, run.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "completed" {
		t.Fatalf("status=%s", done.Status)
	}
	return source, done, manifest
}

// Replay is a load-bearing invariant: a run whose ledger it refuses is a run
// nobody can audit afterwards. A new event type is exactly the kind of change
// that breaks it, in both directions - an unknown type is rejected outright, and
// a type the replayed run does not reproduce makes the two ledgers differ.
//
// The interruption is reproducible because it lives on the recorded turn, so the
// replayed run appends the same event from the record rather than needing a
// provider that hangs again.
func TestRunWithAProviderInterruptionStillReplaysAndCheckpoints(t *testing.T) {
	ctx := context.Background()
	source, run, manifest := interruptedRun(t)
	events, err := source.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	interruptions := 0
	for _, event := range events {
		if event.Type == "provider.interrupted" {
			interruptions++
		}
	}
	if interruptions != 1 {
		t.Fatalf("the fixture did not record an interruption: %d", interruptions)
	}

	report, err := Verify(ctx, source, run.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified {
		t.Fatalf("replay refused a run containing an interruption: %+v", report)
	}

	// Checkpoint discovery walks the same ledger, and an event it does not know
	// stops a fork before it starts.
	points, err := checkpoint.List(ctx, source, run.ID, manifest)
	if err != nil {
		t.Fatalf("checkpoint discovery: %v", err)
	}
	if len(points) == 0 {
		t.Fatal("no checkpoints on a completed run")
	}
}

// Forking rebuilds the prefix event by event into a scratch store and refuses to
// continue if what it rebuilt differs from what was recorded, down to the event
// count. The interrupted attempt is part of that prefix, so a fork past it only
// succeeds if reconstruction reproduces the interruption event.
func TestVerifyInterruptedForkPrefix(t *testing.T) {
	ctx := context.Background()
	source, parent, manifest := interruptedRun(t)
	points, err := checkpoint.List(ctx, source, parent.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	// The last tool response before the end: far enough in that the prefix
	// contains the interrupted turn.
	var selected checkpoint.Checkpoint
	for _, point := range points {
		if point.EventType == "tool.response" {
			selected = point
		}
	}
	if selected.ID == "" {
		t.Fatalf("no tool response checkpoint among %d", len(points))
	}
	created, err := fork.Create(ctx, source, source, selected, manifest, fork.Options{})
	if err != nil {
		t.Fatalf("forking past an interrupted turn: %v", err)
	}
	// The child inherits the transcript, so the turn that needed two attempts
	// still says so in the child's own history.
	var history []agent.Message
	if err := json.Unmarshal([]byte(created.Run.Transcript), &history); err != nil {
		t.Fatal(err)
	}
	carried := 0
	for _, m := range history {
		carried += len(m.Interruptions)
	}
	if carried != 1 {
		t.Fatalf("the reconstructed prefix dropped the interrupted attempt: %s", created.Run.Transcript)
	}
}
