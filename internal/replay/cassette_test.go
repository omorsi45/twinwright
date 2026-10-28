package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"twinwright/internal/agent"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

// cassetteKey is long enough to be masked. The replay never has it.
const cassetteKey = "cassette-test-key-not-a-secret"

const placeholderKey = "cassette-replay-placeholder"

// newCassetteRun seeds the billing world and creates an unexecuted
// duplicate-charge run recorded under the given provider and model, which is what
// a replay reads back to rebuild the adapter.
func newCassetteRun(t *testing.T, provider, model string) (*store.Store, store.Run, compiler.Manifest) {
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
	source, err := store.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	world, err := source.Seed(context.Background(), 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := source.CreateRun(context.Background(), world.ID, "duplicate-charge", provider, model, "Customer C-104 was charged twice. Refund only the duplicate.", "")
	if err != nil {
		t.Fatal(err)
	}
	return source, run, manifest
}

// recordedRun is a completed run whose provider server has been closed, and the
// cassette its wire exchanges were written to.
type recordedRun struct {
	source   *store.Store
	run      store.Run
	manifest compiler.Manifest
	cassette string
	url      string
	waits    []time.Duration
}

// adapterFor builds the adapter under test around a client and a key.
type adapterFor func(serverURL, key string, client *http.Client) agent.Provider

func chatAdapter(serverURL, key string, client *http.Client) agent.Provider {
	return agent.ChatCompletionsProvider{APIKey: key, Model: "local", BaseURL: serverURL + "/v1", Client: client}
}

func responsesAdapter(serverURL, key string, client *http.Client) agent.Provider {
	return agent.OpenAIProvider{APIKey: key, Model: "gpt-test", URL: serverURL, Client: client}
}

// toolThenAnswer is the server of TestChatCompletionsToolRoundTrip, with the
// same two answers: a tool call while the conversation has no tool message yet,
// and a final answer once it has. It also echoes the Authorization header into
// the answer, the way a careless proxy might, so the key reaches a response body.
func toolThenAnswer(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(string(body), `"role":"tool"`) {
			w.Write([]byte(`{"id":"chatcmpl-1","model":"local-server-1","choices":[{"message":{"content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"getCharge","arguments":"{\"id\":\"CH-1002\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
			return
		}
		fmt.Fprintf(w, `{"id":"chatcmpl-2","model":"local-server-1","choices":[{"message":{"content":"Done %s"},"finish_reason":"stop"}]}`, r.Header.Get("Authorization"))
	}
}

// record executes a duplicate-charge run through the adapter, with every wire
// exchange passing through a recording transport that sits on base (nil means
// the default transport), and then closes the server.
func record(t *testing.T, provider, model string, handler http.HandlerFunc, adapter adapterFor, base func(http.RoundTripper) http.RoundTripper) recordedRun {
	t.Helper()
	ctx := context.Background()
	server := httptest.NewServer(handler)
	defer server.Close()
	source, run, manifest := newCassetteRun(t, provider, model)

	var under http.RoundTripper = http.DefaultTransport
	if base != nil {
		under = base(under)
	}
	path := filepath.Join(t.TempDir(), "run.cassette")
	rec, err := agent.NewRecordingTransport(under, path, provider, model, cassetteKey)
	if err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	runner := agent.Runner{
		Store:    source,
		Dispatch: &dispatch.Dispatcher{Store: source, Manifest: manifest},
		Manifest: manifest,
		Provider: adapter(server.URL, cassetteKey, &http.Client{Transport: rec}),
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
	if err = rec.Close(); err != nil {
		t.Fatal(err)
	}
	return recordedRun{source: source, run: done, manifest: manifest, cassette: path, url: server.URL, waits: waits}
}

// replayAdapter rebuilds the run's adapter around the cassette alone, with a
// placeholder key.
func (r recordedRun) replayAdapter(t *testing.T, adapter adapterFor) agent.Provider {
	t.Helper()
	c, err := agent.LoadCassette(r.cassette)
	if err != nil {
		t.Fatal(err)
	}
	return adapter(r.url, placeholderKey, &http.Client{Transport: c.Transport()})
}

// interruptionKinds lists the provider.interrupted events in a run's ledger.
func interruptionKinds(t *testing.T, s *store.Store, runID string) []string {
	t.Helper()
	events, err := s.Events(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, event := range events {
		if event.Type != "provider.interrupted" {
			continue
		}
		var payload struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, payload.Kind)
	}
	return kinds
}

// The cassette is the only thing the replay has: the scripted server is closed
// before it starts, and the key it is given is a placeholder. A verified report
// is the real adapter, run against the recorded bytes, producing the recorded
// ledger event for event.
func TestRecordingThenCassetteReplayReproducesTheLedger(t *testing.T) {
	r := record(t, "openai-compatible", "local", toolThenAnswer(t), chatAdapter, nil)

	report, err := VerifyWith(context.Background(), r.source, r.run.ID, r.manifest, r.replayAdapter(t, chatAdapter))
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified || report.ModelTurns != 2 || report.ToolCalls != 1 {
		t.Fatalf("report = %+v", report)
	}

	// Neither the ledger nor the cassette ever held the key, though the server
	// echoed it into the answer the run stored.
	events, err := r.source.Events(context.Background(), r.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(string(event.Payload), cassetteKey) {
			t.Fatalf("event %d holds the key: %s", event.Seq, event.Payload)
		}
	}
	if !strings.Contains(r.run.Transcript, "Done Bearer [REDACTED]") {
		t.Fatalf("the echoed answer was not stored masked: %s", r.run.Transcript)
	}
	data, err := os.ReadFile(r.cassette)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), cassetteKey) {
		t.Fatalf("the cassette holds the key:\n%s", data)
	}
}

// The Responses API adapter takes the same route, so the guarantee does not rest
// on one wire format.
func TestCassetteReplayVerifiesTheResponsesAdapter(t *testing.T) {
	var served atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if served.Add(1) == 1 {
			w.Write([]byte(`{"id":"resp_1","model":"gpt-test","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"getCustomer","arguments":"{\"id\":\"C-104\"}"}]}`))
			return
		}
		w.Write([]byte(`{"id":"resp_2","model":"gpt-test","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Checked the customer."}]}]}`))
	}
	r := record(t, "openai", "gpt-test", handler, responsesAdapter, nil)
	report, err := VerifyWith(context.Background(), r.source, r.run.ID, r.manifest, r.replayAdapter(t, responsesAdapter))
	if err != nil || !report.Verified {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

// A recorded 429 carries its Retry-After, and the live run waited it out. The
// replay reads the same status and header from the cassette and must not wait:
// the 30 second header makes a replay that did take half a minute instead of
// passing.
func TestCassetteReplayDoesNotWaitOutARecordedRateLimit(t *testing.T) {
	var served atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		switch served.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"slow down"}}`))
		case 2:
			w.Write([]byte(`{"id":"resp_1","model":"gpt-test","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"getCustomer","arguments":"{\"id\":\"C-104\"}"}]}`))
		default:
			w.Write([]byte(`{"id":"resp_2","model":"gpt-test","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Checked the customer."}]}]}`))
		}
	}
	r := record(t, "openai", "gpt-test", handler, responsesAdapter, nil)
	if len(r.waits) != 1 || r.waits[0] != 30*time.Second {
		t.Fatalf("the live run waited %v, want one 30s wait", r.waits)
	}
	if kinds := interruptionKinds(t, r.source, r.run.ID); !reflect.DeepEqual(kinds, []string{"rate_limited"}) {
		t.Fatalf("recorded interruptions = %v", kinds)
	}

	started := time.Now()
	report, err := VerifyWith(context.Background(), r.source, r.run.ID, r.manifest, r.replayAdapter(t, responsesAdapter))
	if err != nil {
		t.Fatal(err)
	}
	if !report.Verified {
		t.Fatalf("report = %+v", report)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("replay took %s, so it waited out the recorded Retry-After", elapsed)
	}
}

// A connection that dropped has no response to record. It is still an exchange,
// and it replays as the same kind of failure: the adapter classifies it as a
// transport interruption again, the retry reads the next exchange, and the ledger
// comes out the same.
func TestCassetteReplayReproducesADroppedConnection(t *testing.T) {
	var calls atomic.Int32
	dropFirst := func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return nil, errors.New("connection reset by peer")
			}
			return next.RoundTrip(req)
		})
	}
	r := record(t, "openai-compatible", "local", toolThenAnswer(t), chatAdapter, dropFirst)
	if kinds := interruptionKinds(t, r.source, r.run.ID); !reflect.DeepEqual(kinds, []string{"transport"}) {
		t.Fatalf("recorded interruptions = %v", kinds)
	}
	c, err := agent.LoadCassette(r.cassette)
	if err != nil {
		t.Fatal(err)
	}
	if first := c.Exchanges[0]; first.Seq != 1 || first.Status != 0 || first.Error == "" {
		t.Fatalf("the dropped connection was not recorded as an exchange: %+v", first)
	}

	report, err := VerifyWith(context.Background(), r.source, r.run.ID, r.manifest, r.replayAdapter(t, chatAdapter))
	if err != nil || !report.Verified {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A cassette that no longer matches the run is a divergence in the report, with
// the exchange and the field named, and not an error: replay's answer is "this
// run does not reproduce", which is a result.
func TestCassetteReplayReportsADivergingCassette(t *testing.T) {
	r := record(t, "openai-compatible", "local", toolThenAnswer(t), chatAdapter, nil)
	data, err := os.ReadFile(r.cassette)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines, want header and two exchanges", len(lines))
	}
	var second struct {
		RequestSHA256 string `json:"request_sha256"`
	}
	if err = json.Unmarshal([]byte(lines[2]), &second); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"request hash changed", []string{lines[0], lines[1], strings.Replace(lines[2], second.RequestSHA256, strings.Repeat("0", 64), 1)}, "seq 2: request_sha256 differs"},
		{"cassette ends early", lines[:2], "seq 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			edited := r
			edited.cassette = filepath.Join(t.TempDir(), "edited.cassette")
			if err := os.WriteFile(edited.cassette, []byte(strings.Join(tc.lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			report, err := VerifyWith(context.Background(), r.source, r.run.ID, r.manifest, edited.replayAdapter(t, chatAdapter))
			if err != nil {
				t.Fatalf("a diverging cassette is a result, not an error: %v", err)
			}
			if report.Verified || !strings.Contains(report.Divergence, tc.want) {
				t.Fatalf("report = %+v, want a divergence saying %q", report, tc.want)
			}
		})
	}
}

// A nil provider is the ledger's own recorded turns, which is Verify.
func TestVerifyWithoutAProviderIsVerify(t *testing.T) {
	source, run, manifest := interruptedRun(t)
	viaVerify, err := Verify(context.Background(), source, run.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	viaWith, err := VerifyWith(context.Background(), source, run.ID, manifest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !viaVerify.Verified || !reflect.DeepEqual(viaVerify, viaWith) {
		t.Fatalf("Verify = %+v, VerifyWith(nil) = %+v", viaVerify, viaWith)
	}
}
