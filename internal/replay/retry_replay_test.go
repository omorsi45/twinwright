package replay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"twinwright/internal/agent"
	"twinwright/internal/checkpoint"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

// newBillingRun seeds a world for the billing example and creates an unexecuted
// duplicate-charge run on an OpenAI-named provider.
func newBillingRun(t *testing.T) (*store.Store, store.Run, compiler.Manifest) {
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
	run, err := source.CreateRun(ctx, world.ID, "duplicate-charge", "openai", "gpt-test", "Customer C-104 was charged twice. Refund only the duplicate.", "")
	if err != nil {
		t.Fatal(err)
	}
	return source, run, manifest
}

// retriedRun is a completed run driven through the real OpenAI adapter, whose
// first attempt at the first turn was refused with a 429 and whose second landed.
//
// It lives here rather than beside the other retry tests because replay imports
// the agent package, so a test inside the agent package cannot import replay.
func retriedRun(t *testing.T, retryAfter string) (*store.Store, store.Run, compiler.Manifest, []time.Duration) {
	t.Helper()
	ctx := context.Background()
	source, run, manifest := newBillingRun(t)

	var served atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch served.Add(1) {
		case 1:
			w.Header().Set("Retry-After", retryAfter)
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"slow down"}}`))
		case 2:
			w.Write([]byte(`{"id":"resp_1","model":"gpt-test","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"getCustomer","arguments":"{\"id\":\"C-104\"}"}]}`))
		default:
			w.Write([]byte(`{"id":"resp_2","model":"gpt-test","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Checked the customer."}]}]}`))
		}
	}))
	t.Cleanup(server.Close)

	var waits []time.Duration
	runner := agent.Runner{
		Store:    source,
		Dispatch: &dispatch.Dispatcher{Store: source, Manifest: manifest},
		Manifest: manifest,
		Provider: agent.OpenAIProvider{APIKey: "test-key-not-a-secret", Model: "gpt-test", URL: server.URL},
		Sleep: func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		},
	}
	done, err := runner.Execute(ctx, run.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "completed" {
		t.Fatalf("status=%s", done.Status)
	}
	if served.Load() != 3 {
		t.Fatalf("%d requests reached the server, want 3 (refused, call, answer)", served.Load())
	}
	return source, done, manifest, waits
}

// A rate-limited attempt is a ledger event, and replay has to reproduce it field
// for field from the recorded turn, status and retry_after_ms included. Replay
// never waits: the interruption is read back from the turn, so the retry wait is
// never reached. The 30 second Retry-After makes a replay that did sleep take
// half a minute instead of passing.
func TestRetriedRunStillReplays(t *testing.T) {
	ctx := context.Background()
	source, run, manifest, waits := retriedRun(t, "30")
	if len(waits) != 1 || waits[0] != 30*time.Second {
		t.Fatalf("the live run waited %v, want one 30s wait", waits)
	}

	events, err := source.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var recorded []json.RawMessage
	for _, event := range events {
		if event.Type == "provider.interrupted" {
			recorded = append(recorded, event.Payload)
		}
	}
	if len(recorded) != 1 || string(recorded[0]) != `{"kind":"rate_limited","attempt":1,"status":429,"retry_after_ms":30000}` {
		t.Fatalf("recorded interruptions = %s", recorded)
	}

	started := time.Now()
	report, err := Verify(ctx, source, run.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified {
		t.Fatalf("replay refused a run containing a rate-limited attempt: %+v", report)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("replay took %s, so it waited out the recorded Retry-After", elapsed)
	}

	// Checkpoint discovery walks the same ledger.
	points, err := checkpoint.List(ctx, source, run.ID, manifest)
	if err != nil {
		t.Fatalf("checkpoint discovery: %v", err)
	}
	if len(points) == 0 {
		t.Fatal("no checkpoints on a completed run")
	}
}
