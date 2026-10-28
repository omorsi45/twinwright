package replay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"twinwright/internal/agent"
	"twinwright/internal/agent/exectest"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/eval"
	"twinwright/internal/store"
)

// TestMain turns this test binary into an exec agent process when the provider
// starts it with exectest.Mode set. The test lives here and not beside the
// provider because it must call Verify, and internal/agent cannot import this
// package.
func TestMain(m *testing.M) {
	exectest.MaybeRun()
	os.Exit(m.Run())
}

// An agent that is a separate process makes its decisions from the history it is
// sent, is started afresh for every turn, and still leaves a ledger that replays
// without it: the recorded turns are what replay reads, so verification needs no
// agent, no command and no environment.
func TestExecProviderCompletesTheBillingRun(t *testing.T) {
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
	s, err := store.Open(filepath.Join(t.TempDir(), "world.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	world, err := s.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	// The charge listing fails once, so the agent has to read a 503 and retry.
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "exec", "helper@test", "Customer C-104 was charged twice. Refund only the duplicate.", "listCharges")
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	provider := agent.ExecProvider{
		Command: []string{exe},
		Env:     []string{exectest.Mode + "=billing"},
		Timeout: 30 * time.Second,
	}
	runner := agent.Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: provider}

	paused, err := runner.Execute(ctx, run.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Status != "paused" {
		t.Fatalf("status after three turns = %s, want paused", paused.Status)
	}
	done, err := runner.Execute(ctx, run.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "completed" {
		t.Fatalf("status = %s, want completed", done.Status)
	}
	report, err := eval.Evaluate(ctx, s, world.ID, "duplicate-charge")
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Fatalf("evaluation = %+v", report)
	}

	verified, err := Verify(ctx, s, run.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !verified.Verified || verified.ModelTurns == 0 || verified.ToolCalls == 0 {
		t.Fatalf("replay = %+v", verified)
	}

	// The command and the environment names are the operator's, not the run's:
	// nothing in the run row or the ledger may carry them.
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	record := done.Transcript
	for _, event := range events {
		record += string(event.Payload)
	}
	for _, private := range []string{exe, exectest.Mode} {
		if strings.Contains(record, private) {
			t.Fatalf("the ledger records %q", private)
		}
	}
}
