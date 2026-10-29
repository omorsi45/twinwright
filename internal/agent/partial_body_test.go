package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ADR 0031 says a body that fails part way is an ordinary error and not an
// interruption, so the run fails on the first attempt and spends no second call.
// Nothing pinned that at the adapter: the cassette test covers only the recorder.
func TestBodyFailurePartWayIsNotRetried(t *testing.T) {
	for _, tc := range adapterCases() {
		t.Run(tc.name, func(t *testing.T) {
			var served atomic.Int32
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				served.Add(1)
				io.Copy(io.Discard, r.Body)
				// A body shorter than it announced: the connection closes with the
				// client still waiting for the rest.
				w.Header().Set("Content-Length", "400")
				w.Write([]byte(`{"id":"x","choices":`))
			})
			out := executeWith(t, tc.build(server.URL, 0))
			if out.err == nil || out.run.Status != "failed" {
				t.Fatalf("a body cut short must fail the run: err=%v status=%s", out.err, out.run.Status)
			}
			if served.Load() != 1 {
				t.Fatalf("%d requests reached the server, want 1: a cut body is not retried", served.Load())
			}
			var interrupted *CallInterrupted
			if errors.As(out.err, &interrupted) {
				t.Fatalf("a body that failed part way was classified as an interruption: %+v", interrupted)
			}
			wantWaits(t, out.waits)
			wantInterruptions(t, recordedInterruptions(t, out.s, out.run.ID))
		})
	}
}

// A deadline that passes while the body is being read is recorded as a 200 with an
// error, because the headers had already arrived. Replayed, it is the same cut
// body, so it fails the run as an ordinary error and is not retried. Only a
// deadline that passes before the headers is a transport interruption on replay,
// which is the case ADR 0031 describes.
func TestRecordedBodyFailureReplaysAsAnOrdinaryError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Length", "400")
		w.Write([]byte(`{"id":"x","choices":`))
	}))
	path := filepath.Join(t.TempDir(), "cut.cassette")
	rec, err := NewRecordingTransport(http.DefaultTransport, path, "openai-compatible", "m")
	if err != nil {
		t.Fatal(err)
	}
	live := ChatCompletionsProvider{Model: "m", BaseURL: server.URL + "/v1", Client: &http.Client{Transport: rec}}
	if _, err = live.Next(context.Background(), "task", nil, cassetteOps); err == nil {
		t.Fatal("the live call succeeded on a cut body")
	}
	if err = rec.Close(); err != nil {
		t.Fatal(err)
	}
	server.Close()

	c := loadCassette(t, path)
	if len(c.Exchanges) != 1 || c.Exchanges[0].Status != http.StatusOK || c.Exchanges[0].Error == "" {
		t.Fatalf("exchanges = %+v, want one 200 that carries the error", c.Exchanges)
	}
	replay := ChatCompletionsProvider{APIKey: "cassette-replay-placeholder", Model: "m", BaseURL: server.URL + "/v1", Client: &http.Client{Transport: c.Transport()}}
	_, err = replay.Next(context.Background(), "task", nil, cassetteOps)
	var interrupted *CallInterrupted
	if err == nil || errors.As(err, &interrupted) {
		t.Fatalf("replay err = %v, want an ordinary error and not an interruption", err)
	}
}

// Anthropic documents 529 as overloaded_error, a temporary condition, which is
// what a retry exists for. The other two adapters do not give 529 that meaning,
// so it stays an ordinary error there (TestClientErrorIsNotRetried).
func TestAnthropicOverloadedIsRetriedOnce(t *testing.T) {
	anthropic := adapterCases()[2]
	if anthropic.name != "anthropic" {
		t.Fatalf("adapterCases()[2] is %s, not anthropic", anthropic.name)
	}
	out := runAgainst(t, anthropic, reply{status: 529}, reply{status: 200})
	wantCompleted(t, out)
	wantInterruptions(t, recordedInterruptions(t, out.s, out.run.ID), `{"kind":"server_error","attempt":1,"status":529}`)
	wantWaits(t, out.waits, time.Second)
	if out.requests != 2 {
		t.Fatalf("%d requests reached the server, want 2", out.requests)
	}

	t.Run("twice fails the run with the status", func(t *testing.T) {
		out := runAgainst(t, anthropic, reply{status: 529}, reply{status: 529}, reply{status: 200})
		if out.err == nil || out.run.Status != "failed" || out.requests != 2 {
			t.Fatalf("err=%v status=%s requests=%d, want a failed run after exactly two attempts", out.err, out.run.Status, out.requests)
		}
		if !strings.Contains(out.err.Error(), "2 of 2 attempts") || !strings.Contains(out.err.Error(), strconv.Itoa(529)) {
			t.Fatalf("the failure lost what the provider said: %v", out.err)
		}
	})

	// The classification is the adapter's, not the status code's: the shared list
	// is unchanged.
	if got := statusInterruption(&http.Response{StatusCode: 529, Header: http.Header{}}, errors.New("cause")); got != nil {
		t.Fatalf("529 is an interruption for every adapter: %+v", got)
	}
	got := statusInterruption(&http.Response{StatusCode: 529, Header: http.Header{}}, errors.New("cause"), anthropicOverloaded)
	if got == nil || got.Kind != InterruptServerError || got.Status != 529 {
		t.Fatalf("with the Anthropic status = %+v, want a server_error", got)
	}
}
