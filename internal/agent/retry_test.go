package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"twinwright/internal/compiler"
	"twinwright/internal/store"
)

// reply is one scripted answer from a fake provider server. A 200 gets the
// adapter's own success body, so a retry that lands is a real, usable turn.
type reply struct {
	status     int
	retryAfter string
}

// replyServer answers with the replies in order and then repeats the last one.
// The counter is how many requests reached it, which is how a test tells a retry
// that happened from one that was only recorded.
func replyServer(t *testing.T, tc adapterCase, replies ...reply) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var served atomic.Int32
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		answer := replies[min(int(served.Add(1)), len(replies))-1]
		if answer.retryAfter != "" {
			w.Header().Set("Retry-After", answer.retryAfter)
		}
		if answer.status == http.StatusOK {
			w.Write([]byte(strings.ReplaceAll(tc.success, "ECHO", "ok")))
			return
		}
		w.WriteHeader(answer.status)
		w.Write([]byte(`{"error":{"message":"try later"}}`))
	})
	return server, &served
}

// sleepRecorder stands in for Runner.Sleep: a retry wait costs no time, and the
// length the runner asked for can be asserted.
type sleepRecorder struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (s *sleepRecorder) Sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waits = append(s.waits, d)
	return nil
}

func (s *sleepRecorder) recorded() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.waits)
}

// retryOutcome is what a run left behind after its provider misbehaved.
type retryOutcome struct {
	s        *store.Store
	run      store.Run
	err      error
	requests int
	waits    []time.Duration
}

func executeWith(t *testing.T, provider Provider) retryOutcome {
	t.Helper()
	ctx := context.Background()
	s, runner, run := interruptRun(t)
	var sleeps sleepRecorder
	runner.Sleep = sleeps.Sleep
	runner.Provider = provider
	_, err := runner.Execute(ctx, run.ID, 20)
	final, getErr := s.Run(ctx, run.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	return retryOutcome{s: s, run: final, err: err, waits: sleeps.recorded()}
}

func runAgainst(t *testing.T, tc adapterCase, replies ...reply) retryOutcome {
	t.Helper()
	server, served := replyServer(t, tc, replies...)
	out := executeWith(t, tc.build(server.URL, 0))
	out.requests = int(served.Load())
	return out
}

// recordedInterruptions are the provider.interrupted payloads in the ledger, as
// stored.
func recordedInterruptions(t *testing.T, s *store.Store, runID string) [][]byte {
	t.Helper()
	events, err := s.Events(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for _, event := range events {
		if event.Type == "provider.interrupted" {
			out = append(out, []byte(event.Payload))
		}
	}
	return out
}

func wantInterruptions(t *testing.T, got [][]byte, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("recorded %d interruptions %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if !equalJSON(got[i], []byte(want[i])) {
			t.Fatalf("interruption %d = %s, want %s", i+1, got[i], want[i])
		}
	}
}

func wantWaits(t *testing.T, got []time.Duration, want ...time.Duration) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("retry waits = %v, want %v", got, want)
	}
}

func wantCompleted(t *testing.T, out retryOutcome) {
	t.Helper()
	if out.err != nil {
		t.Fatalf("the run failed: %v", out.err)
	}
	if out.run.Status != "completed" {
		t.Fatalf("status = %s, want completed", out.run.Status)
	}
}

// A 429 is the provider saying "not now", which is routinely gone a moment
// later. The attempt that was refused goes in the ledger with the status and the
// wait the server asked for, so a run that took two calls does not look like one
// that took one.
func TestRateLimitedAttemptIsRetriedAndRecorded(t *testing.T) {
	for _, tc := range adapterCases() {
		t.Run(tc.name, func(t *testing.T) {
			out := runAgainst(t, tc, reply{status: 429, retryAfter: "2"}, reply{status: 200})
			wantCompleted(t, out)
			wantInterruptions(t, recordedInterruptions(t, out.s, out.run.ID),
				`{"kind":"rate_limited","attempt":1,"status":429,"retry_after_ms":2000}`)
			wantWaits(t, out.waits, 2*time.Second)
			if out.requests != 2 {
				t.Fatalf("%d requests reached the server, want 2", out.requests)
			}
		})
	}
}

// A Retry-After that cannot be used counts as not sent: a wait of zero would
// retry straight into the same limit, and a past date is not a wait at all.
func TestUnusableRetryAfterFallsBackToOneSecond(t *testing.T) {
	past := time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC).Format(http.TimeFormat)
	for _, value := range []string{"", "0", "-3", "soon", "1.5", past} {
		t.Run("Retry-After "+strconv.Quote(value), func(t *testing.T) {
			out := runAgainst(t, adapterCases()[0], reply{status: 429, retryAfter: value}, reply{status: 200})
			wantCompleted(t, out)
			wantInterruptions(t, recordedInterruptions(t, out.s, out.run.ID),
				`{"kind":"rate_limited","attempt":1,"status":429}`)
			wantWaits(t, out.waits, time.Second)
		})
	}
}

// A server error is retried once, never more: the second failure is a different
// signal, and a third attempt only delays the report while spending another call.
func TestServerErrorIsRetriedOnce(t *testing.T) {
	for _, tc := range adapterCases() {
		for _, status := range []int{500, 502, 503, 504} {
			t.Run(tc.name+"/"+strconv.Itoa(status), func(t *testing.T) {
				out := runAgainst(t, tc, reply{status: status}, reply{status: 200})
				wantCompleted(t, out)
				wantInterruptions(t, recordedInterruptions(t, out.s, out.run.ID),
					`{"kind":"server_error","attempt":1,"status":`+strconv.Itoa(status)+`}`)
				wantWaits(t, out.waits, time.Second)
				if out.requests != 2 {
					t.Fatalf("%d requests reached the server, want 2", out.requests)
				}
			})
		}
		for _, status := range []int{429, 503} {
			t.Run(tc.name+"/twice "+strconv.Itoa(status), func(t *testing.T) {
				out := runAgainst(t, tc, reply{status: status}, reply{status: status}, reply{status: 200})
				if out.err == nil || out.run.Status != "failed" {
					t.Fatalf("a call interrupted on both attempts must fail the run: err=%v status=%s", out.err, out.run.Status)
				}
				if out.requests != 2 {
					t.Fatalf("%d requests reached the server, want exactly the two attempts", out.requests)
				}
				// One wait, between the two attempts. Nothing follows the last.
				wantWaits(t, out.waits, time.Second)
				kind := InterruptServerError
				if status == 429 {
					kind = InterruptRateLimited
				}
				wantInterruptions(t, recordedInterruptions(t, out.s, out.run.ID),
					`{"kind":"`+kind+`","attempt":1,"status":`+strconv.Itoa(status)+`}`,
					`{"kind":"`+kind+`","attempt":2,"status":`+strconv.Itoa(status)+`}`)
				if !strings.Contains(out.err.Error(), "2 of 2 attempts") || !strings.Contains(out.err.Error(), strconv.Itoa(status)) {
					t.Fatalf("the failure lost what the provider said: %v", out.err)
				}
			})
		}
	}
}

// A server can ask for any wait it likes. The runner honours it only up to a
// bound, because a run that sleeps for an hour on one header is a hung run.
func TestRetryAfterIsCappedAndCancellable(t *testing.T) {
	t.Run("seconds are capped", func(t *testing.T) {
		out := runAgainst(t, adapterCases()[0], reply{status: 429, retryAfter: "120"}, reply{status: 200})
		wantCompleted(t, out)
		wantWaits(t, out.waits, 30*time.Second)
		// The record carries the capped value, so what the ledger says the run
		// waited is what it waited.
		wantInterruptions(t, recordedInterruptions(t, out.s, out.run.ID),
			`{"kind":"rate_limited","attempt":1,"status":429,"retry_after_ms":30000}`)
	})
	t.Run("an HTTP date is capped too", func(t *testing.T) {
		later := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
		out := runAgainst(t, adapterCases()[0], reply{status: 503, retryAfter: later}, reply{status: 200})
		wantCompleted(t, out)
		wantWaits(t, out.waits, 30*time.Second)
	})
	t.Run("the default sleep returns at once for a cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// Run in a goroutine so a sleep that ignores the context fails this test
		// in seconds instead of holding the whole suite for an hour.
		done := make(chan error, 1)
		go func() { done <- Runner{}.sleep(ctx, time.Hour) }()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a sleep on a cancelled context did not return")
		}
	})
	t.Run("the default sleep does wait when nothing cancels it", func(t *testing.T) {
		started := time.Now()
		if err := (Runner{}).sleep(context.Background(), 30*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed < 30*time.Millisecond {
			t.Fatalf("slept %s, want at least 30ms", elapsed)
		}
	})
	t.Run("a run cancelled during the wait stops without a second attempt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		provider := &cancelOnFirstCall{cancel: cancel, interrupt: &CallInterrupted{Kind: InterruptRateLimited, Status: 429, RetryAfter: MaxRetryAfter}}
		started := time.Now()
		_, err := Runner{Provider: provider}.callProvider(ctx, "task", nil, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the context error", err)
		}
		if provider.calls != 1 {
			t.Fatalf("%d attempts, want 1: nobody is waiting for a second", provider.calls)
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("the wait was not cut short: %s", elapsed)
		}
	})
}

// cancelOnFirstCall cancels the run while its first attempt is failing, which is
// what a caller going away during a backoff looks like.
type cancelOnFirstCall struct {
	cancel    context.CancelFunc
	interrupt *CallInterrupted
	calls     int
}

func (p *cancelOnFirstCall) Next(context.Context, string, []Message, []compiler.Operation) (Message, error) {
	p.calls++
	p.cancel()
	return Message{}, p.interrupt
}

// A rejected key or a malformed request is not something waiting fixes, and two
// identical refusals are worse evidence than one. Only the listed transient
// statuses are interruptions; the unlisted 5xx codes are ordinary errors too.
func TestClientErrorIsNotRetried(t *testing.T) {
	for _, tc := range adapterCases() {
		for _, status := range []int{400, 401, 403, 404, 422, 501, 505, 529} {
			if status == 529 && tc.name == "anthropic" {
				// Anthropic's documented overloaded status: a temporary condition,
				// retried once (TestAnthropicOverloadedIsRetriedOnce).
				continue
			}
			t.Run(tc.name+"/"+strconv.Itoa(status), func(t *testing.T) {
				out := runAgainst(t, tc, reply{status: status}, reply{status: 200})
				if out.err == nil || out.run.Status != "failed" {
					t.Fatalf("err=%v status=%s", out.err, out.run.Status)
				}
				if out.requests != 1 {
					t.Fatalf("%d requests reached the server, want 1", out.requests)
				}
				wantWaits(t, out.waits)
				wantInterruptions(t, recordedInterruptions(t, out.s, out.run.ID))
				var interrupted *CallInterrupted
				if errors.As(out.err, &interrupted) {
					t.Fatalf("status %d was classified as an interruption: %+v", status, interrupted)
				}
				if !strings.Contains(out.err.Error(), "HTTP "+strconv.Itoa(status)) {
					t.Fatalf("the error lost the status: %v", out.err)
				}
			})
		}
	}
}

// The classification is a closed list, so it is pinned across every status code
// rather than at the few the other tests happen to use.
func TestOnlyTheTransientStatusesAreInterruptions(t *testing.T) {
	for status := 100; status <= 599; status++ {
		got := statusInterruption(&http.Response{StatusCode: status, Header: http.Header{}}, errors.New("cause"))
		want := ""
		switch status {
		case 429:
			want = InterruptRateLimited
		case 500, 502, 503, 504:
			want = InterruptServerError
		}
		switch {
		case want == "" && got != nil:
			t.Fatalf("status %d was classified as %q", status, got.Kind)
		case want != "" && (got == nil || got.Kind != want || got.Status != status):
			t.Fatalf("status %d = %+v, want kind %q", status, got, want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"2", 2 * time.Second},
		{" 7 ", 7 * time.Second},
		{"120", 120 * time.Second},
		{"-1", 0},
		{"+5", 0},
		{"1.5", 0},
		{"soon", 0},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{now.Add(-time.Hour).Format(http.TimeFormat), 0},
		{now.Format(http.TimeFormat), 0},
	} {
		if got := parseRetryAfter(tc.value, now); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
	// A number too large for a duration is still "a long time", not a wrapped
	// negative that would count as unsent.
	for _, huge := range []string{"9223372036854775807", "99999999999999999999999999"} {
		if got := parseRetryAfter(huge, now); got < MaxRetryAfter {
			t.Errorf("parseRetryAfter(%q) = %v, want at least the cap", huge, got)
		}
	}
}

func TestRecordCapsTheServersRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   CallInterrupted
		want Interruption
	}{
		{"capped", CallInterrupted{Kind: InterruptRateLimited, Status: 429, RetryAfter: 120 * time.Second},
			Interruption{Kind: InterruptRateLimited, Attempt: 1, Status: 429, RetryAfterMS: 30000}},
		{"under the cap", CallInterrupted{Kind: InterruptServerError, Status: 503, RetryAfter: 2 * time.Second},
			Interruption{Kind: InterruptServerError, Attempt: 1, Status: 503, RetryAfterMS: 2000}},
		{"none sent", CallInterrupted{Kind: InterruptRateLimited, Status: 429},
			Interruption{Kind: InterruptRateLimited, Attempt: 1, Status: 429}},
		{"transport has no status", CallInterrupted{Kind: InterruptTransport},
			Interruption{Kind: InterruptTransport, Attempt: 1}},
	} {
		if got := tc.in.Record(1); got != tc.want {
			t.Errorf("%s: Record = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// Timeouts and truncations are retried at once, as before: a hung call already
// spent its deadline, and a cut-short completion is not rate pressure.
func TestTimeoutsAndTruncationsStillRetryWithoutWaiting(t *testing.T) {
	for _, kind := range []string{InterruptTimeout, InterruptTruncated} {
		t.Run(kind, func(t *testing.T) {
			remaining := 1
			out := executeWith(t, interruptThen{remaining: &remaining, interrupt: &CallInterrupted{Kind: kind, Reason: "length", Deadline: time.Second}})
			wantCompleted(t, out)
			wantWaits(t, out.waits)
			if got := recordedInterruptions(t, out.s, out.run.ID); len(got) != 1 {
				t.Fatalf("recorded %d interruptions, want 1", len(got))
			}
		})
	}
}

// droppingTransport fails the first `failures` round trips the way a dropped
// connection does, and then forwards.
type droppingTransport struct {
	failures atomic.Int32
	cause    error
	next     http.RoundTripper
}

func (f *droppingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.failures.Add(-1) >= 0 {
		return nil, f.cause
	}
	return f.next.RoundTrip(r)
}

func withClient(t *testing.T, p Provider, client *http.Client) Provider {
	t.Helper()
	switch built := p.(type) {
	case OpenAIProvider:
		built.Client = client
		return built
	case ChatCompletionsProvider:
		built.Client = client
		return built
	case AnthropicProvider:
		built.Client = client
		return built
	}
	t.Fatalf("unexpected provider %T", p)
	return nil
}

func TestTransportFailureIsRetriedAndRecorded(t *testing.T) {
	for _, tc := range adapterCases() {
		t.Run(tc.name, func(t *testing.T) {
			server, served := replyServer(t, tc, reply{status: 200})
			transport := &droppingTransport{cause: errors.New("connection reset by peer"), next: http.DefaultTransport}
			transport.failures.Store(1)
			provider := withClient(t, tc.build(server.URL, 0), &http.Client{Transport: transport})
			out := executeWith(t, provider)
			wantCompleted(t, out)
			wantInterruptions(t, recordedInterruptions(t, out.s, out.run.ID), `{"kind":"transport","attempt":1}`)
			wantWaits(t, out.waits, time.Second)
			if served.Load() != 1 {
				t.Fatalf("%d requests reached the server, want only the retry", served.Load())
			}
		})
	}
}

// What a transport failure prints is what the ledger records, so it has to be
// the redacted text: the URL's query and userinfo and the configured key must
// not survive into the ledger, the transcript or the returned error.
func TestTransportFailureNeverLeaksIntoTheLedger(t *testing.T) {
	const query, userinfo = "query-key-token", "userinfo-pw-token"
	for _, tc := range adapterCases() {
		t.Run(tc.name+"/dial", func(t *testing.T) {
			out := executeWith(t, tc.build("http://user:"+userinfo+"@127.0.0.1:1/v1?key="+query, 0))
			assertNoLeak(t, out, "dial", query, userinfo, secretKey)
		})
		t.Run(tc.name+"/key in the cause", func(t *testing.T) {
			transport := &droppingTransport{cause: errors.New("proxy rejected " + secretKey), next: http.DefaultTransport}
			transport.failures.Store(99)
			provider := withClient(t, tc.build("http://127.0.0.1:1/v1?key="+query, 0), &http.Client{Transport: transport})
			assertNoLeak(t, executeWith(t, provider), "cause", query, userinfo, secretKey)
		})
	}
}

func assertNoLeak(t *testing.T, out retryOutcome, what string, secrets ...string) {
	t.Helper()
	if out.err == nil || out.run.Status != "failed" {
		t.Fatalf("%s: both attempts failed, so the run must fail: err=%v status=%s", what, out.err, out.run.Status)
	}
	wantWaits(t, out.waits, time.Second)
	got := recordedInterruptions(t, out.s, out.run.ID)
	wantInterruptions(t, got, `{"kind":"transport","attempt":1}`, `{"kind":"transport","attempt":2}`)
	events, err := out.s.Events(context.Background(), out.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	everything := out.err.Error() + "\n" + out.run.Transcript
	for _, event := range events {
		everything += "\n" + string(event.Payload)
	}
	for _, secret := range secrets {
		if strings.Contains(everything, secret) {
			t.Fatalf("%s: %q reached the ledger, transcript or error:\n%s", what, secret, everything)
		}
	}
	if !strings.Contains(out.err.Error(), "2 of 2 attempts") {
		t.Fatalf("%s: err = %v", what, out.err)
	}
}
