package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The tests in this file prove that the endpoint this package serves is one a
// real Prometheus can scrape, and that what it scrapes can be queried back. The
// rest of the package asserts the bytes of the exposition document, which shows
// the format is what the documentation says and not that a scraper accepts it.
//
// The gated test needs the Compose stack (docker compose up -d prometheus) and is
// opt-in, so go test ./... stays hermetic and binds nothing. The ungated tests
// hold the part that does not need a stack: reading Prometheus's query answer and
// polling for it.

const (
	// scrapePollInterval is short because deploy/prometheus.yml scrapes every 2
	// seconds, so a sample is normally queryable within a few seconds of the
	// listener starting.
	scrapePollInterval = 500 * time.Millisecond
	// scrapePollTimeout is the longest a first scrape may take.
	scrapePollTimeout = 60 * time.Second
	// scrapeReadLimit bounds how much of an answer is read.
	scrapeReadLimit = 8 << 20
	// scrapeBodyLimit is how much of the last answer a failure message prints.
	scrapeBodyLimit = 4 << 10

	// scrapeAddress is where the gated test serves metrics. deploy/prometheus.yml
	// names host.docker.internal:9095 as the target.
	//
	// It binds every interface, unlike the loopback default the worker documents,
	// and only here. The Prometheus container reaches the host through the Docker
	// bridge gateway, which a loopback-only listener does not answer. The listener
	// lives for one test, serves one store holding one queued run, and the CI job
	// that sets TWINWRIGHT_TEST_OTEL=1 runs on a throwaway runner.
	scrapeAddress = "0.0.0.0:9095"

	// scrapeWorker labels the worker's own counters so the test can tell that the
	// series it found came from the listener it started.
	scrapeWorker = "prometheus-scrape-test"
)

// promSample is one series of an instant-vector query answer.
type promSample struct {
	Metric map[string]string `json:"metric"`
	// Value is [timestamp, "value"]: Prometheus writes the value as a string.
	Value []any `json:"value"`
}

func (s promSample) number() (float64, error) {
	if len(s.Value) != 2 {
		return 0, fmt.Errorf("a sample value has %d elements, want 2", len(s.Value))
	}
	text, ok := s.Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("a sample value is %T, want a string", s.Value[1])
	}
	return strconv.ParseFloat(text, 64)
}

// decodeQuery reads the answer to GET /api/v1/query: {"status":"success","data":
// {"resultType":"vector","result":[{"metric":{...},"value":[ts,"1"]}]}}. Only
// those fields are decoded.
func decodeQuery(body []byte) ([]promSample, error) {
	var answer struct {
		Status    string `json:"status"`
		Error     string `json:"error"`
		ErrorType string `json:"errorType"`
		Data      struct {
			ResultType string       `json:"resultType"`
			Result     []promSample `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("the answer is not Prometheus's query JSON: %w", err)
	}
	if answer.Status != "success" {
		return nil, fmt.Errorf("the query failed: status %q, %s: %s", answer.Status, answer.ErrorType, answer.Error)
	}
	if answer.Data.ResultType != "vector" {
		return nil, fmt.Errorf("the result type is %q, want vector", answer.Data.ResultType)
	}
	return answer.Data.Result, nil
}

// findSample returns the value of the first sample whose labels include every
// pair in want, and whether there was one.
func findSample(samples []promSample, want map[string]string) (float64, bool, error) {
	for _, sample := range samples {
		matches := true
		for key, value := range want {
			if sample.Metric[key] != value {
				matches = false
				break
			}
		}
		if matches {
			value, err := sample.number()
			return value, true, err
		}
	}
	return 0, false, nil
}

func scrapeExcerpt(body []byte) string {
	if len(body) > scrapeBodyLimit {
		return string(body[:scrapeBodyLimit]) + "..."
	}
	return string(body)
}

// query runs one instant query and returns the series and the raw answer.
func query(ctx context.Context, client *http.Client, base, expression string) ([]promSample, []byte, error) {
	address := strings.TrimRight(base, "/") + "/api/v1/query?query=" + url.QueryEscape(expression)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("GET %s: %w", address, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, scrapeReadLimit))
	if err != nil {
		return nil, body, fmt.Errorf("read the answer to %s: %w", address, err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, body, fmt.Errorf("GET %s answered HTTP %d", address, response.StatusCode)
	}
	samples, err := decodeQuery(body)
	return samples, body, err
}

// checkScrape asks Prometheus for the two series the test depends on. It returns
// nil when both are what the served store implies: a runnable queue entry count
// of 1, and this worker's claims counter present. Otherwise it returns the
// problem and the answer that showed it, for a failure message.
func checkScrape(ctx context.Context, client *http.Client, base string) ([]byte, error) {
	samples, body, err := query(ctx, client, base, "twinwright_queue_entries")
	if err != nil {
		return body, err
	}
	value, found, err := findSample(samples, map[string]string{"state": "runnable"})
	if err != nil {
		return body, err
	}
	if !found {
		return body, fmt.Errorf(`no twinwright_queue_entries sample has state="runnable"`)
	}
	if value != 1 {
		return body, fmt.Errorf(`twinwright_queue_entries{state="runnable"} is %v, want 1`, value)
	}

	samples, body, err = query(ctx, client, base, "twinwright_worker_claims_total")
	if err != nil {
		return body, err
	}
	_, found, err = findSample(samples, map[string]string{"worker": scrapeWorker})
	if err != nil {
		return body, err
	}
	if !found {
		return body, fmt.Errorf("no twinwright_worker_claims_total sample has worker=%q", scrapeWorker)
	}
	return nil, nil
}

// waitForScrape polls checkScrape until it passes, the listener stops or the
// timeout passes. stopped is closed when the listener returns. A timeout reports
// the last problem and the last answer (truncated).
func waitForScrape(ctx context.Context, client *http.Client, base string, stopped <-chan struct{}, interval, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-stopped:
			return fmt.Errorf("the metrics listener stopped before Prometheus scraped it")
		default:
		}
		body, problem := checkScrape(ctx, client, base)
		if problem == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no good answer within %s: %w\nlast body: %s", timeout, problem, scrapeExcerpt(body))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// TestPrometheusScrapesAWorker serves a worker's metrics over a store holding one
// queued run and waits for a real Prometheus to scrape them.
func TestPrometheusScrapesAWorker(t *testing.T) {
	if os.Getenv("TWINWRIGHT_TEST_OTEL") != "1" {
		t.Skip("set TWINWRIGHT_TEST_OTEL=1 to run against the Compose observability stack (docker compose up -d prometheus)")
	}
	base := "http://127.0.0.1:9090"
	if value := strings.TrimSpace(os.Getenv("TW_PROMETHEUS_URL")); value != "" {
		base = value
	}

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

	serveCtx, stop := context.WithCancel(ctx)
	stopped := make(chan struct{})
	var serveErr error
	go func() {
		defer close(stopped)
		serveErr = Serve(serveCtx, scrapeAddress, Collector{Store: s, Counters: NewCounters(), WorkerID: scrapeWorker})
	}()
	t.Cleanup(func() {
		stop()
		<-stopped
	})

	pollCtx, cancel := context.WithTimeout(ctx, scrapePollTimeout+30*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 5 * time.Second}
	if err = waitForScrape(pollCtx, client, base, stopped, scrapePollInterval, scrapePollTimeout); err != nil {
		select {
		case <-stopped:
			// serveErr is written before stopped is closed, so reading it here is ordered.
			t.Fatalf("Prometheus at %s did not scrape %s: %v (the listener returned: %v)", base, scrapeAddress, err, serveErr)
		default:
			t.Fatalf("Prometheus at %s did not scrape %s: %v", base, scrapeAddress, err)
		}
	}
}

// fakePrometheus answers instant queries from a function, so the polling can be
// tested without a Prometheus.
func fakePrometheus(t *testing.T, answers func(expression string, call int32) (int, string)) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("path = %q", r.URL.Path)
		}
		status, body := answers(r.URL.Query().Get("query"), calls.Add(1))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func vector(samples ...string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[` + strings.Join(samples, ",") + `]}}`
}

const (
	runnableOne = `{"metric":{"__name__":"twinwright_queue_entries","instance":"host.docker.internal:9095","job":"twinwright-workers","state":"runnable"},"value":[1730000000.5,"1"]}`
	runnableTwo = `{"metric":{"__name__":"twinwright_queue_entries","state":"runnable"},"value":[1730000000.5,"2"]}`
	leasedOne   = `{"metric":{"__name__":"twinwright_queue_entries","state":"leased"},"value":[1730000000.5,"1"]}`
	claimsZero  = `{"metric":{"__name__":"twinwright_worker_claims_total","worker":"prometheus-scrape-test"},"value":[1730000000.5,"0"]}`
)

func TestDecodeQuery(t *testing.T) {
	samples, err := decodeQuery([]byte(vector(runnableOne, leasedOne)))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 {
		t.Fatalf("samples = %d", len(samples))
	}
	value, found, err := findSample(samples, map[string]string{"state": "runnable"})
	if err != nil || !found || value != 1 {
		t.Fatalf("runnable = %v found=%v err=%v", value, found, err)
	}
	if _, found, _ = findSample(samples, map[string]string{"state": "failed"}); found {
		t.Fatal("a state with no sample was found")
	}

	empty, err := decodeQuery([]byte(vector()))
	if err != nil || len(empty) != 0 {
		t.Fatalf("an empty vector: samples=%v err=%v", empty, err)
	}
	for name, body := range map[string]string{
		"not json":         "<html>",
		"an error":         `{"status":"error","errorType":"bad_data","error":"parse error"}`,
		"a scalar":         `{"status":"success","data":{"resultType":"scalar","result":[1730000000.5,"1"]}}`,
		"truncated answer": `{"status":"success","data":{"resultType":"vec`,
	} {
		if _, err := decodeQuery([]byte(body)); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// Prometheus writes a sample's value as a string. A number there is not the
// documented shape and must not be read as one.
func TestSampleValueMustBeAString(t *testing.T) {
	samples, err := decodeQuery([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"state":"runnable"},"value":[1730000000.5,1]}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = findSample(samples, map[string]string{"state": "runnable"}); err == nil {
		t.Fatal("a numeric value was read")
	}
}

// Prometheus has no sample until its first scrape and answers a successful empty
// vector in the meantime. The poll has to wait that out and then require both
// series.
func TestWaitForScrapeOutlastsTheFirstScrape(t *testing.T) {
	server := fakePrometheus(t, func(expression string, call int32) (int, string) {
		if call <= 4 {
			return http.StatusOK, vector()
		}
		switch expression {
		case "twinwright_queue_entries":
			return http.StatusOK, vector(leasedOne, runnableOne)
		case "twinwright_worker_claims_total":
			return http.StatusOK, vector(claimsZero)
		}
		t.Errorf("unexpected query %q", expression)
		return http.StatusBadRequest, ""
	})
	stopped := make(chan struct{})
	if err := waitForScrape(context.Background(), server.Client(), server.URL, stopped, 5*time.Millisecond, 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

// Each case is a scrape that happened and still is not what the served store
// implies, or no scrape at all.
func TestWaitForScrapeRefusesWhatIsNotTheServedStore(t *testing.T) {
	for _, tc := range []struct {
		name    string
		queue   string
		claims  string
		problem string
	}{
		{"no scrape yet", vector(), vector(), `no twinwright_queue_entries sample has state="runnable"`},
		{"two runnable entries", vector(runnableTwo), vector(claimsZero), `is 2, want 1`},
		{"another state only", vector(leasedOne), vector(claimsZero), `no twinwright_queue_entries sample has state="runnable"`},
		{"no claims counter", vector(runnableOne), vector(), `no twinwright_worker_claims_total sample has worker="prometheus-scrape-test"`},
		{"another worker's counter", vector(runnableOne),
			vector(`{"metric":{"worker":"someone-else"},"value":[1730000000.5,"0"]}`), `no twinwright_worker_claims_total sample`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := fakePrometheus(t, func(expression string, _ int32) (int, string) {
				if expression == "twinwright_queue_entries" {
					return http.StatusOK, tc.queue
				}
				return http.StatusOK, tc.claims
			})
			stopped := make(chan struct{})
			err := waitForScrape(context.Background(), server.Client(), server.URL, stopped, 5*time.Millisecond, 50*time.Millisecond)
			if err == nil {
				t.Fatal("a wrong scrape was accepted")
			}
			if !strings.Contains(err.Error(), tc.problem) || !strings.Contains(err.Error(), "last body: ") {
				t.Fatalf("err = %q, want it to mention %q and the last body", err, tc.problem)
			}
		})
	}
}

func TestWaitForScrapeStopsWhenTheListenerDies(t *testing.T) {
	server := fakePrometheus(t, func(string, int32) (int, string) { return http.StatusOK, vector() })
	stopped := make(chan struct{})
	close(stopped)
	err := waitForScrape(context.Background(), server.Client(), server.URL, stopped, 5*time.Millisecond, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "listener stopped") {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitForScrapeTruncatesTheLastBody(t *testing.T) {
	padding := strings.Repeat("x", 3*scrapeBodyLimit)
	server := fakePrometheus(t, func(string, int32) (int, string) {
		return http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[],"note":"` + padding + `"}}`
	})
	stopped := make(chan struct{})
	err := waitForScrape(context.Background(), server.Client(), server.URL, stopped, 5*time.Millisecond, 50*time.Millisecond)
	if err == nil {
		t.Fatal("an empty vector was accepted")
	}
	message := err.Error()
	if len(message) > 2*scrapeBodyLimit || !strings.HasSuffix(message, "...") {
		t.Fatalf("the failure message is %d bytes and ends %q", len(message), message[len(message)-10:])
	}
}

func TestWaitForScrapeReportsAnUnreachablePrometheus(t *testing.T) {
	server := fakePrometheus(t, func(string, int32) (int, string) { return http.StatusOK, vector() })
	address := server.URL
	server.Close()
	stopped := make(chan struct{})
	err := waitForScrape(context.Background(), &http.Client{Timeout: time.Second}, address, stopped, 5*time.Millisecond, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "GET "+address) {
		t.Fatalf("err = %v", err)
	}
}
