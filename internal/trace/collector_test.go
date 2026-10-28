package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"twinwright/internal/agent"
	"twinwright/internal/store"
)

// The tests in this file prove the claim the OTLP exporter exists to make: that
// the spans it posts are accepted by a real collector, pass through it to a
// real backend, and can be read back by trace ID. Every other test in the
// package answers the exporter from an httptest server, which shows the bytes
// are sent and proves nothing about what a collector does with them.
//
// The gated test needs the Compose stack (docker compose up -d otel-collector
// jaeger) and is opt-in, so go test ./... stays hermetic. The ungated tests
// hold the part of it that does not need a stack: the comparison of Jaeger's
// answer with the local trace, and the polling around it. They are there so the
// assertions have been seen to fail, which a test that only ever runs in CI
// could not otherwise show.

const (
	// collectorPollInterval is short because the collector batches for one
	// second and Jaeger indexes after that, so a trace is normally readable
	// within a few seconds of the export.
	collectorPollInterval = 500 * time.Millisecond
	// collectorPollTimeout is the longest a trace may take to become readable.
	collectorPollTimeout = 60 * time.Second
	// collectorReadLimit bounds how much of an answer is read. A trace of a whole
	// run is far larger than a failure message should be, so reading and
	// printing have separate limits.
	collectorReadLimit = 8 << 20
	// collectorBodyLimit is how much of the last answer a failure message prints.
	collectorBodyLimit = 4 << 10
)

// jaegerTraces decodes only the fields of Jaeger's query API that the test
// asserts: GET /api/traces/{traceID} answers {"data":[{"traceID":...,"spans":
// [{"operationName":...,"tags":[...],"references":[...]}]}]}.
type jaegerTraces struct {
	Data []struct {
		TraceID string       `json:"traceID"`
		Spans   []jaegerSpan `json:"spans"`
	} `json:"data"`
}

type jaegerSpan struct {
	OperationName string `json:"operationName"`
	Tags          []struct {
		Key string `json:"key"`
	} `json:"tags"`
	References []struct {
		RefType string `json:"refType"`
	} `json:"references"`
}

func (s jaegerSpan) hasTag(key string) bool {
	for _, tag := range s.Tags {
		if tag.Key == key {
			return true
		}
	}
	return false
}

// isRoot reports whether the span has no parent. A reference of another type,
// such as the FOLLOWS_FROM link a fork carries to its parent's root, does not
// make a span a child.
func (s jaegerSpan) isRoot() bool {
	for _, reference := range s.References {
		if reference.RefType == "CHILD_OF" {
			return false
		}
	}
	return true
}

// checkJaegerTrace compares Jaeger's answer with the trace built locally. It
// returns nil when the answer holds the same number of spans, one root whose
// operation name is the local root's name, and at least one span carrying the
// ledger sequence tag. Anything else is an error that says what differed, so a
// run that times out can print why the last answer was not good enough.
func checkJaegerTrace(body []byte, local Trace) error {
	var answer jaegerTraces
	if err := json.Unmarshal(body, &answer); err != nil {
		return fmt.Errorf("the answer is not Jaeger's trace JSON: %w", err)
	}
	if len(answer.Data) != 1 {
		return fmt.Errorf("the answer holds %d traces, want 1", len(answer.Data))
	}
	trace := answer.Data[0]
	if trace.TraceID != local.TraceID {
		return fmt.Errorf("the answer is for trace %q, want %q", trace.TraceID, local.TraceID)
	}
	if want := countSpans(local); len(trace.Spans) != want {
		return fmt.Errorf("the answer holds %d spans, want %d", len(trace.Spans), want)
	}
	var roots []jaegerSpan
	tagged := false
	for _, span := range trace.Spans {
		if span.isRoot() {
			roots = append(roots, span)
		}
		tagged = tagged || span.hasTag("twinwright.event.start_seq")
	}
	if len(roots) != 1 {
		return fmt.Errorf("the answer holds %d spans with no CHILD_OF reference, want 1 root", len(roots))
	}
	if roots[0].OperationName != local.Root.Name {
		return fmt.Errorf("the root span is %q, want %q", roots[0].OperationName, local.Root.Name)
	}
	if !tagged {
		return fmt.Errorf("no span carries the tag twinwright.event.start_seq")
	}
	return nil
}

// excerpt is the part of an answer a failure message prints.
func excerpt(body []byte) string {
	if len(body) > collectorBodyLimit {
		return string(body[:collectorBodyLimit]) + "..."
	}
	return string(body)
}

// getBody returns the status and at most collectorReadLimit bytes of the answer
// to a GET.
func getBody(ctx context.Context, client *http.Client, url string) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, collectorReadLimit))
	return response.StatusCode, body, err
}

// waitForJaegerTrace polls url until Jaeger's answer passes checkJaegerTrace or
// the timeout passes. A timeout reports the last problem and the last answer
// (truncated), because a CI log is all there is to diagnose from.
func waitForJaegerTrace(ctx context.Context, client *http.Client, url string, local Trace, interval, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastProblem error
	var lastBody []byte
	for {
		status, body, err := getBody(ctx, client, url)
		switch {
		case err != nil:
			lastProblem, lastBody = fmt.Errorf("GET %s: %w", url, err), nil
		case status != http.StatusOK:
			// 404 is the normal answer until the spans have been indexed.
			lastProblem, lastBody = fmt.Errorf("GET %s answered HTTP %d", url, status), body
		default:
			if lastProblem = checkJaegerTrace(body, local); lastProblem == nil {
				return nil
			}
			lastBody = body
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no good answer within %s: %w\nlast body: %s", timeout, lastProblem, excerpt(lastBody))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func otelEnv(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

// TestSpansAreQueryableInJaeger exports a scripted company-incident run through
// the collector and reads it back from Jaeger by trace ID.
func TestSpansAreQueryableInJaeger(t *testing.T) {
	if os.Getenv("TWINWRIGHT_TEST_OTEL") != "1" {
		t.Skip("set TWINWRIGHT_TEST_OTEL=1 to run against the Compose observability stack (docker compose up -d otel-collector jaeger)")
	}
	endpoint := otelEnv("TW_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	jaeger := strings.TrimRight(otelEnv("TW_JAEGER_URL", "http://127.0.0.1:16686"), "/")

	s := newStore(t)
	run := execute(t, s, companyManifest(t), "company-incident", "fixture-v1", store.RunOptions{},
		agent.CompanyScriptedProvider{Scenario: "company-incident"}, 25)
	if run.Status != "completed" {
		t.Fatalf("the scripted run ended %q, want completed", run.Status)
	}
	local := build(t, s, run.ID)

	ctx, cancel := context.WithTimeout(context.Background(), collectorPollTimeout+30*time.Second)
	defer cancel()
	delivery, err := Exporter{Endpoint: endpoint}.Export(ctx, local)
	if err != nil {
		t.Fatalf("export to %s: %v", endpoint, err)
	}
	if delivery.TraceID != local.TraceID || delivery.Spans != countSpans(local) {
		t.Fatalf("delivery = %+v for trace %s with %d spans", delivery, local.TraceID, countSpans(local))
	}

	url := jaeger + "/api/traces/" + local.TraceID
	client := &http.Client{Timeout: 5 * time.Second}
	if err := waitForJaegerTrace(ctx, client, url, local, collectorPollInterval, collectorPollTimeout); err != nil {
		t.Fatalf("trace %s is not queryable in Jaeger: %v", local.TraceID, err)
	}
}

// goodJaegerBody is what Jaeger answers for sampleTrace once the spans are
// indexed: a root named run and two children that point at it. mutate may change
// the spans, or return a different list, before they are encoded.
func goodJaegerBody(t *testing.T, local Trace, mutate func(spans []map[string]any) []map[string]any) []byte {
	t.Helper()
	tag := map[string]any{"key": "twinwright.event.start_seq", "type": "int64", "value": 1}
	child := []any{map[string]any{"refType": "CHILD_OF", "traceID": local.TraceID, "spanID": local.Root.SpanID}}
	spans := []map[string]any{
		{"operationName": "run", "tags": []any{tag}, "references": []any{}},
		{"operationName": "model", "tags": []any{tag}, "references": child},
		{"operationName": "tool", "tags": []any{tag}, "references": child},
	}
	if mutate != nil {
		spans = mutate(spans)
	}
	body, err := json.Marshal(map[string]any{"data": []any{map[string]any{"traceID": local.TraceID, "spans": spans}}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestCheckJaegerTraceAcceptsWhatTheExporterSent(t *testing.T) {
	local := sampleTrace()
	if err := checkJaegerTrace(goodJaegerBody(t, local, nil), local); err != nil {
		t.Fatal(err)
	}
}

// Each case is one way a collector or a backend could hand back something that
// is not the trace that was sent. The check has to refuse every one.
func TestCheckJaegerTraceRefusesWhatIsNotTheTrace(t *testing.T) {
	local := sampleTrace()
	mutated := func(change func(spans []map[string]any) []map[string]any) func(*testing.T) []byte {
		return func(t *testing.T) []byte { return goodJaegerBody(t, local, change) }
	}
	for _, tc := range []struct {
		name   string
		body   func(t *testing.T) []byte
		reason string
	}{
		{"not json", func(*testing.T) []byte { return []byte("<html>") }, "not Jaeger's trace JSON"},
		{"no trace", func(*testing.T) []byte {
			return []byte(`{"data":null,"errors":[{"code":404,"msg":"trace not found"}]}`)
		}, "holds 0 traces"},
		{"another trace", func(t *testing.T) []byte {
			other := local
			other.TraceID = "ffffffffffffffffffffffffffffffff"
			return goodJaegerBody(t, other, nil)
		}, "is for trace"},
		{"a span is missing", mutated(func(spans []map[string]any) []map[string]any { return spans[:2] }), "holds 2 spans, want 3"},
		{"a span is duplicated", mutated(func(spans []map[string]any) []map[string]any { return append(spans, spans[2]) }), "holds 4 spans, want 3"},
		{"the root is named something else", mutated(func(spans []map[string]any) []map[string]any {
			spans[0]["operationName"] = "model.invocation"
			return spans
		}), `the root span is "model.invocation", want "run"`},
		{"no root", mutated(func(spans []map[string]any) []map[string]any {
			spans[0]["references"] = spans[1]["references"]
			return spans
		}), "holds 0 spans with no CHILD_OF reference"},
		{"two roots", mutated(func(spans []map[string]any) []map[string]any {
			spans[1]["references"] = []any{}
			return spans
		}), "holds 2 spans with no CHILD_OF reference"},
		{"a link does not make a child", mutated(func(spans []map[string]any) []map[string]any {
			spans[1]["references"] = []any{map[string]any{"refType": "FOLLOWS_FROM"}}
			return spans
		}), "holds 2 spans with no CHILD_OF reference"},
		{"no span carries the ledger tag", mutated(func(spans []map[string]any) []map[string]any {
			for _, span := range spans {
				span["tags"] = []any{map[string]any{"key": "http.status", "type": "int64", "value": 200}}
			}
			return spans
		}), "twinwright.event.start_seq"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkJaegerTrace(tc.body(t), local)
			if err == nil {
				t.Fatal("a wrong answer was accepted")
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("err = %q, want it to mention %q", err, tc.reason)
			}
		})
	}
}

// Jaeger answers 404 until the spans are indexed, so a 404 has to be waited
// out and a later good answer has to pass.
func TestWaitForJaegerTraceOutlastsTheNotFoundAnswer(t *testing.T) {
	local := sampleTrace()
	var served atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/traces/"+local.TraceID {
			t.Errorf("path = %q", r.URL.Path)
		}
		if served.Add(1) <= 2 {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"code":404,"msg":"trace not found"}]}`))
			return
		}
		_, _ = w.Write(goodJaegerBody(t, local, nil))
	}))
	defer server.Close()
	url := server.URL + "/api/traces/" + local.TraceID
	if err := waitForJaegerTrace(context.Background(), server.Client(), url, local, 5*time.Millisecond, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if served.Load() != 3 {
		t.Fatalf("served %d answers, want 3", served.Load())
	}
}

// A timeout has to leave something to diagnose from: the reason the last answer
// was refused and the start of that answer, bounded.
func TestWaitForJaegerTraceTimeoutReportsTheLastAnswer(t *testing.T) {
	local := sampleTrace()
	padding := strings.Repeat("x", 3*collectorBodyLimit)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"data":[],"note":"%s"}`, padding)
	}))
	defer server.Close()
	err := waitForJaegerTrace(context.Background(), server.Client(), server.URL, local, 5*time.Millisecond, 50*time.Millisecond)
	if err == nil {
		t.Fatal("a trace that never arrived was reported as found")
	}
	message := err.Error()
	if !strings.Contains(message, "holds 0 traces") || !strings.Contains(message, `last body: {"data":[],"note":"xxx`) {
		t.Fatalf("the failure does not say why: %.300s", message)
	}
	if len(message) > 2*collectorBodyLimit {
		t.Fatalf("the failure message is %d bytes; the body was not truncated", len(message))
	}
	if !strings.HasSuffix(message, "...") {
		t.Fatalf("a truncated body is not marked: %s", message[len(message)-100:])
	}
}

// A real trace is many times longer than a failure message may be. The reader
// must not cut an answer at the print limit, or a good answer fails to decode.
func TestWaitForJaegerTraceReadsAnAnswerLargerThanThePrintLimit(t *testing.T) {
	local := sampleTrace()
	body := goodJaegerBody(t, local, func(spans []map[string]any) []map[string]any {
		spans[1]["logs"] = []any{map[string]any{"fields": []any{map[string]any{"key": "note", "value": strings.Repeat("y", 10*collectorBodyLimit)}}}}
		return spans
	})
	if len(body) <= collectorBodyLimit {
		t.Fatalf("the fixture is %d bytes, not larger than the print limit", len(body))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer server.Close()
	if err := waitForJaegerTrace(context.Background(), server.Client(), server.URL, local, 5*time.Millisecond, time.Second); err != nil {
		t.Fatal(err)
	}
}
