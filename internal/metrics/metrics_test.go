package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"twinwright/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/metrics.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRenderReportsQueueRunAndLedgerState(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	world, err := s.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Enqueue(ctx, run.ID, 10, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err = s.Append(ctx, run.ID, "tool.response", map[string]any{"call_id": "c1"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Append(ctx, run.ID, "authorization.denied", map[string]any{"reason": "scope"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AcquireRunLease(ctx, run.ID, "worker-a", time.Minute, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	counters := NewCounters()
	counters.Add(WorkerClaims, "", 3)
	counters.Add(WorkerDispositions, "finished", 2)
	counters.Add(WorkerDispositions, "fenced", 1)

	body, err := Collector{Store: s, Counters: counters, WorkerID: "worker-a"}.Render(ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		`twinwright_worker_claims_total{worker="worker-a"} 3`,
		`twinwright_worker_claim_disposition_total{disposition="fenced",worker="worker-a"} 1`,
		`twinwright_worker_claim_disposition_total{disposition="finished",worker="worker-a"} 2`,
		`twinwright_queue_entries{state="runnable"} 1`,
		`twinwright_queue_entries{state="finished"} 0`,
		`twinwright_runs{status="running"} 1`,
		`twinwright_tool_calls 1`,
		`twinwright_authorization_denials 1`,
		`twinwright_ownership_events{kind="lease.acquired"} 1`,
		`twinwright_workers_registered 0`,
		"# TYPE twinwright_queue_entries gauge",
		"# HELP twinwright_runs ",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output is missing %q\n---\n%s", want, body)
		}
	}
	// States with no rows must still be present as zeros: a series that
	// disappears when it hits zero breaks alerting on "queue drained".
	if !strings.Contains(body, `twinwright_queue_entries{state="failed"} 0`) {
		t.Fatalf("zero-valued series were omitted\n%s", body)
	}
}

func TestHandlerServesPrometheusContentType(t *testing.T) {
	s := testStore(t)
	server := httptest.NewServer(Collector{Store: s, Counters: NewCounters(), WorkerID: "w"}.Handler())
	defer server.Close()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("content type=%q", got)
	}
}

// A scrape that cannot read the database must fail loudly. Returning a partial
// document would be parsed as every series dropping to zero, which reads as an
// outage that is not happening.
func TestHandlerFailsRatherThanReturningPartialMetrics(t *testing.T) {
	s := testStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(Collector{Store: s, Counters: NewCounters(), WorkerID: "w"}.Handler())
	defer server.Close()
	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", response.StatusCode)
	}
}

func TestCountersAreSafeWhenNil(t *testing.T) {
	var counters *Counters
	counters.Add(WorkerClaims, "", 1) // must not panic
	body, err := Collector{Counters: counters, WorkerID: "w"}.Render(context.Background())
	if err != nil {
		t.Fatalf("render without a store: %v", err)
	}
	if !strings.Contains(body, "# TYPE twinwright_worker_claims_total counter") {
		t.Fatalf("expected the metric declarations even with no data\n%s", body)
	}
}

func TestServeShutsDownWithTheContext(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, "127.0.0.1:0", Collector{Store: s, Counters: NewCounters(), WorkerID: "w"})
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}
}
