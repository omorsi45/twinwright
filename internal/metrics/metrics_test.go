package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sort"
	"strconv"
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

// ServeListener answers on the listener it is given and closes it when the
// context ends, and Serve reports a bind failure to its caller instead of
// swallowing it in a goroutine.
func TestServeListenerServesOnTheListenerItWasGiven(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeListener(ctx, listener, Collector{Counters: NewCounters(), WorkerID: "w"})
	}()

	for path, want := range map[string]string{"/healthz": "ok", "/metrics": "twinwright_worker_claims_total"} {
		response, err := http.Get("http://" + address + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Fatalf("GET %s = %d %q, want 200 and %q", path, response.StatusCode, body, want)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeListener returned %v after its context ended", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeListener did not return after its context ended")
	}
	if conn, err := net.DialTimeout("tcp", address, time.Second); err == nil {
		conn.Close()
		t.Fatal("the listener is still accepting connections after ServeListener returned")
	}
}

func TestServeReportsAnAddressThatCannotBeBound(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	address := held.Addr().String()
	err = Serve(context.Background(), address, Collector{Counters: NewCounters(), WorkerID: "w"})
	if err == nil || !strings.Contains(err.Error(), address) {
		t.Fatalf("Serve on a taken address returned %v, want an error naming %s", err, address)
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

// expoSample is one sample line of a parsed exposition document.
type expoSample struct {
	name   string
	labels map[string]string
	value  float64
}

// expoFamily is one metric family: its HELP, its TYPE and the samples written
// under them.
type expoFamily struct {
	name    string
	help    string
	kind    string
	samples []expoSample
}

var (
	metricNamePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNamePattern  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// parseExposition is a strict reader for the Prometheus text format as this
// package writes it. It accepts only `# HELP`, `# TYPE` and sample lines, and
// it enforces what a scraper relies on: HELP then TYPE come before a family's
// samples, a family is written once and contiguously, label values use only the
// three defined escapes, and no sample repeats.
func parseExposition(body string) ([]*expoFamily, error) {
	if body == "" || !strings.HasSuffix(body, "\n") {
		return nil, errors.New("a document must end with a newline")
	}
	var families []*expoFamily
	byName := map[string]*expoFamily{}
	seen := map[string]bool{}
	var current *expoFamily
	for n, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		fail := func(format string, args ...any) ([]*expoFamily, error) {
			return nil, fmt.Errorf("line %d %q: %s", n+1, line, fmt.Sprintf(format, args...))
		}
		switch {
		case strings.HasPrefix(line, "# HELP "):
			name, help, ok := strings.Cut(strings.TrimPrefix(line, "# HELP "), " ")
			if !ok || help == "" || !metricNamePattern.MatchString(name) {
				return fail("malformed HELP")
			}
			if byName[name] != nil {
				return fail("family %s is declared twice", name)
			}
			current = &expoFamily{name: name, help: help}
			byName[name] = current
			families = append(families, current)
		case strings.HasPrefix(line, "# TYPE "):
			name, kind, ok := strings.Cut(strings.TrimPrefix(line, "# TYPE "), " ")
			if !ok || (kind != "counter" && kind != "gauge" && kind != "histogram") {
				return fail("malformed TYPE")
			}
			family := byName[name]
			if family == nil || family != current {
				return fail("TYPE must directly follow the HELP of the same family")
			}
			if family.kind != "" {
				return fail("TYPE given twice")
			}
			family.kind = kind
		case strings.HasPrefix(line, "#"):
			return fail("only HELP and TYPE comments are allowed")
		default:
			sample, err := parseSampleLine(line)
			if err != nil {
				return fail("%v", err)
			}
			family := familyFor(byName, sample.name)
			switch {
			case family == nil:
				return fail("sample of a family with no HELP and TYPE")
			case family.kind == "":
				return fail("sample before its family's TYPE")
			case family != current:
				return fail("family %s is not contiguous", family.name)
			}
			key := sample.name + fmt.Sprint(sample.labels)
			if seen[key] {
				return fail("duplicate sample")
			}
			seen[key] = true
			family.samples = append(family.samples, sample)
		}
	}
	for _, family := range families {
		if family.kind == "" {
			return nil, fmt.Errorf("family %s has no TYPE", family.name)
		}
	}
	return families, nil
}

// familyFor maps a sample name to its family: the name itself, or for a
// histogram the _bucket, _sum and _count series of its base name.
func familyFor(byName map[string]*expoFamily, sample string) *expoFamily {
	if family := byName[sample]; family != nil && family.kind != "histogram" {
		return family
	}
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if base, ok := strings.CutSuffix(sample, suffix); ok {
			if family := byName[base]; family != nil && family.kind == "histogram" {
				return family
			}
		}
	}
	return nil
}

func parseSampleLine(line string) (expoSample, error) {
	end := strings.IndexAny(line, "{ ")
	if end <= 0 {
		return expoSample{}, errors.New("no sample name")
	}
	sample := expoSample{name: line[:end], labels: map[string]string{}}
	if !metricNamePattern.MatchString(sample.name) {
		return expoSample{}, fmt.Errorf("bad metric name %q", sample.name)
	}
	rest := line[end:]
	if rest[0] == '{' {
		rest = rest[1:]
		for {
			eq := strings.IndexByte(rest, '=')
			if eq <= 0 || !labelNamePattern.MatchString(rest[:eq]) {
				return expoSample{}, errors.New("bad label name")
			}
			name := rest[:eq]
			if _, dup := sample.labels[name]; dup {
				return expoSample{}, fmt.Errorf("label %s repeated", name)
			}
			rest = rest[eq+1:]
			if rest == "" || rest[0] != '"' {
				return expoSample{}, errors.New("label value is not quoted")
			}
			var value strings.Builder
			i := 1
			for ; i < len(rest) && rest[i] != '"'; i++ {
				if rest[i] != '\\' {
					value.WriteByte(rest[i])
					continue
				}
				i++
				if i >= len(rest) {
					return expoSample{}, errors.New("dangling escape")
				}
				switch rest[i] {
				case '\\', '"':
					value.WriteByte(rest[i])
				case 'n':
					value.WriteByte('\n')
				default:
					return expoSample{}, fmt.Errorf("undefined escape \\%c", rest[i])
				}
			}
			if i >= len(rest) {
				return expoSample{}, errors.New("unterminated label value")
			}
			sample.labels[name] = value.String()
			rest = rest[i+1:]
			if strings.HasPrefix(rest, ",") {
				rest = rest[1:]
				continue
			}
			if strings.HasPrefix(rest, "}") {
				rest = rest[1:]
				break
			}
			return expoSample{}, errors.New("labels are not closed")
		}
	}
	if !strings.HasPrefix(rest, " ") || strings.ContainsAny(rest[1:], " \t") {
		return expoSample{}, errors.New("expected exactly one space and a value")
	}
	value, err := strconv.ParseFloat(rest[1:], 64)
	if err != nil {
		return expoSample{}, fmt.Errorf("bad value: %v", err)
	}
	sample.value = value
	return sample, nil
}

func mustParse(t *testing.T, body string) map[string]*expoFamily {
	t.Helper()
	families, err := parseExposition(body)
	if err != nil {
		t.Fatalf("%v\n---\n%s", err, body)
	}
	byName := map[string]*expoFamily{}
	for _, family := range families {
		byName[family.name] = family
	}
	return byName
}

// sampleValue returns the value of the sample with exactly these labels.
func sampleValue(t *testing.T, family *expoFamily, name string, labels map[string]string) float64 {
	t.Helper()
	if family == nil {
		t.Fatalf("no family for sample %s%v", name, labels)
	}
	for _, sample := range family.samples {
		if sample.name == name && reflect.DeepEqual(sample.labels, labels) {
			return sample.value
		}
	}
	t.Fatalf("family %s has no sample %s%v; samples=%v", family.name, name, labels, family.samples)
	return 0
}

func TestExpositionParserRejectsMalformedDocuments(t *testing.T) {
	for name, body := range map[string]string{
		"families interleaved":        "# HELP a x\n# TYPE a counter\n# HELP b x\n# TYPE b counter\na 1\nb 1\n",
		"sample before TYPE":          "# HELP a x\na 1\n# TYPE a counter\n",
		"TYPE without HELP":           "# TYPE a counter\na 1\n",
		"sample of an unknown family": "# HELP a x\n# TYPE a counter\nb 1\n",
		"family declared twice":       "# HELP a x\n# TYPE a counter\na 1\n# HELP a x\n# TYPE a counter\n",
		"stray comment":               "# HELP a x\n# TYPE a counter\n# note\na 1\n",
		"blank line":                  "# HELP a x\n# TYPE a counter\n\na 1\n",
		"duplicate sample":            "# HELP a x\n# TYPE a counter\na 1\na 1\n",
		"unquoted label":              "# HELP a x\n# TYPE a counter\na{l=1} 1\n",
		"undefined escape":            "# HELP a x\n# TYPE a counter\na{l=\"\\q\"} 1\n",
		"unclosed labels":             "# HELP a x\n# TYPE a counter\na{l=\"v\" 1\n",
		"no trailing newline":         "# HELP a x\n# TYPE a counter\na 1",
		"histogram base sample":       "# HELP a x\n# TYPE a histogram\na 1\n",
	} {
		if _, err := parseExposition(body); err == nil {
			t.Errorf("%s: the strict parser accepted %q", name, body)
		}
	}
	good := "# HELP a x\n# TYPE a counter\na{l=\"q\\\"\\\\\\n\"} 1\n"
	if _, err := parseExposition(good); err != nil {
		t.Errorf("the strict parser rejected a valid document: %v", err)
	}
}

var allDispositions = []string{"failed", "fenced", "finished", "requeued"}

var allCounters = []string{
	WorkerClaims, WorkerDispositions, WorkerLeaseRenewals,
	WorkerFencingRejections, WorkerTakeovers, WorkerRunSeconds,
}

// A counter that appears only after its first event cannot be alerted on or
// rate()d across a worker restart: the first increment has no earlier sample to
// measure from. Every counter therefore exists at zero from the first scrape.
func TestCountersRenderZeroBeforeFirstUse(t *testing.T) {
	for name, counters := range map[string]*Counters{"empty": NewCounters(), "nil": nil} {
		t.Run(name, func(t *testing.T) {
			body, err := Collector{Counters: counters, WorkerID: "w"}.Render(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			families := mustParse(t, body)
			for _, metric := range allCounters {
				family := families[metric]
				if family == nil || family.kind != "counter" {
					t.Fatalf("%s is not declared as a counter\n%s", metric, body)
				}
				if metric == WorkerDispositions {
					continue
				}
				if value := sampleValue(t, family, metric, map[string]string{"worker": "w"}); value != 0 {
					t.Fatalf("%s=%v before any event", metric, value)
				}
			}
			dispositions := families[WorkerDispositions]
			if len(dispositions.samples) != len(allDispositions) {
				t.Fatalf("dispositions render %d series before any claim, want %d\n%s", len(dispositions.samples), len(allDispositions), body)
			}
			for _, disposition := range allDispositions {
				if value := sampleValue(t, dispositions, WorkerDispositions, map[string]string{"disposition": disposition, "worker": "w"}); value != 0 {
					t.Fatalf("disposition %s=%v before any claim", disposition, value)
				}
			}
		})
	}
}

func TestZeroSamplesGiveWayToCounts(t *testing.T) {
	counters := NewCounters()
	counters.Add(WorkerClaims, "", 2)
	counters.Add(WorkerDispositions, "finished", 2)
	counters.Add(WorkerRunSeconds, "", 1.5)
	body, err := Collector{Counters: counters, WorkerID: "w"}.Render(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	families := mustParse(t, body)
	worker := map[string]string{"worker": "w"}
	if got := sampleValue(t, families[WorkerClaims], WorkerClaims, worker); got != 2 {
		t.Fatalf("claims=%v", got)
	}
	if got := sampleValue(t, families[WorkerRunSeconds], WorkerRunSeconds, worker); got != 1.5 {
		t.Fatalf("run seconds=%v", got)
	}
	if got := sampleValue(t, families[WorkerTakeovers], WorkerTakeovers, worker); got != 0 {
		t.Fatalf("an untouched counter moved: %v", got)
	}
	finished := map[string]string{"disposition": "finished", "worker": "w"}
	requeued := map[string]string{"disposition": "requeued", "worker": "w"}
	if got := sampleValue(t, families[WorkerDispositions], WorkerDispositions, finished); got != 2 {
		t.Fatalf("finished=%v", got)
	}
	if got := sampleValue(t, families[WorkerDispositions], WorkerDispositions, requeued); got != 0 {
		t.Fatalf("requeued=%v", got)
	}
	if len(families[WorkerDispositions].samples) != len(allDispositions) {
		t.Fatalf("a count added a series: %v", families[WorkerDispositions].samples)
	}
}

// The label name belongs to the metric, not to whichever value happens to be
// passed. Inferring it from a non-empty value is what once gave a claims
// counter a disposition label.
func TestCounterLabelNamesAreDeclaredPerMetric(t *testing.T) {
	counters := NewCounters()
	for _, metric := range allCounters {
		counters.Add(metric, "", 1)
	}
	for _, disposition := range allDispositions {
		counters.Add(WorkerDispositions, disposition, 1)
	}
	// A label passed to an unlabelled counter must not create a label on it,
	// and a labelled counter with no label has no series to count against.
	counters.Add(WorkerClaims, "finished", 1)
	counters.Add("twinwright_worker_undeclared_total", "", 1)

	body, err := Collector{Counters: counters, WorkerID: "w"}.Render(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	families := mustParse(t, body)
	if _, ok := families["twinwright_worker_undeclared_total"]; ok {
		t.Fatal("an undeclared counter was rendered")
	}
	for _, metric := range allCounters {
		for _, sample := range families[metric].samples {
			want := []string{"worker"}
			if metric == WorkerDispositions {
				want = []string{"disposition", "worker"}
			}
			var got []string
			for name := range sample.labels {
				got = append(got, name)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s labels=%v want %v", metric, got, want)
			}
			if sample.labels["worker"] != "w" {
				t.Errorf("%s lost the worker label: %v", metric, sample.labels)
			}
		}
	}
	if got := sampleValue(t, families[WorkerClaims], WorkerClaims, map[string]string{"worker": "w"}); got != 2 {
		t.Fatalf("claims=%v want both adds counted on the one series", got)
	}
}

func TestDispositionLabelsAreTheDeclaredSet(t *testing.T) {
	got := DispositionLabels()
	if !reflect.DeepEqual(got, allDispositions) {
		t.Fatalf("DispositionLabels=%v want %v", got, allDispositions)
	}
	got[0] = "tampered"
	if DispositionLabels()[0] != "failed" {
		t.Fatal("DispositionLabels returns the package's own slice")
	}
}

var wantBuckets = []string{"0.05", "0.1", "0.25", "0.5", "1", "2.5", "5", "10", "30", "60", "120", "+Inf"}

func bucketSeries(t *testing.T, family *expoFamily, worker string) (counts []float64, bounds []string) {
	t.Helper()
	for _, sample := range family.samples {
		if sample.name != WorkerRunDuration+"_bucket" {
			continue
		}
		if sample.labels["worker"] != worker {
			t.Fatalf("bucket without the worker label: %v", sample.labels)
		}
		counts = append(counts, sample.value)
		bounds = append(bounds, sample.labels["le"])
	}
	return counts, bounds
}

func TestRunDurationHistogramIsCumulative(t *testing.T) {
	counters := NewCounters()
	observed := []float64{0.01, 0.05, 0.3, 7, 200}
	total := 0.0
	for _, seconds := range observed {
		counters.Observe(WorkerRunDuration, seconds)
		total += seconds
	}
	body, err := Collector{Counters: counters, WorkerID: "w"}.Render(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	family := mustParse(t, body)[WorkerRunDuration]
	if family == nil || family.kind != "histogram" {
		t.Fatalf("%s is not declared as a histogram\n%s", WorkerRunDuration, body)
	}
	counts, bounds := bucketSeries(t, family, "w")
	if !reflect.DeepEqual(bounds, wantBuckets) {
		t.Fatalf("buckets=%v want %v", bounds, wantBuckets)
	}
	// An upper bound is inclusive: 0.05 belongs to the le="0.05" bucket.
	wantCounts := []float64{2, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 5}
	if !reflect.DeepEqual(counts, wantCounts) {
		t.Fatalf("cumulative counts=%v want %v", counts, wantCounts)
	}
	for i := 1; i < len(counts); i++ {
		if counts[i] < counts[i-1] {
			t.Fatalf("bucket counts decrease at %s: %v", bounds[i], counts)
		}
	}
	worker := map[string]string{"worker": "w"}
	count := sampleValue(t, family, WorkerRunDuration+"_count", worker)
	if counts[len(counts)-1] != count || count != float64(len(observed)) {
		t.Fatalf("+Inf=%v count=%v want %d", counts[len(counts)-1], count, len(observed))
	}
	if sum := sampleValue(t, family, WorkerRunDuration+"_sum", worker); math.Abs(sum-total) > 1e-9 {
		t.Fatalf("sum=%v want %v", sum, total)
	}
}

func TestRunDurationHistogramRendersBeforeAnyObservation(t *testing.T) {
	body, err := Collector{Counters: NewCounters(), WorkerID: "w"}.Render(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	family := mustParse(t, body)[WorkerRunDuration]
	if family == nil {
		t.Fatalf("no histogram family\n%s", body)
	}
	counts, bounds := bucketSeries(t, family, "w")
	if !reflect.DeepEqual(bounds, wantBuckets) {
		t.Fatalf("buckets=%v want %v", bounds, wantBuckets)
	}
	for i, count := range counts {
		if count != 0 {
			t.Fatalf("bucket %s=%v before any observation", bounds[i], count)
		}
	}
	worker := map[string]string{"worker": "w"}
	if sampleValue(t, family, WorkerRunDuration+"_sum", worker) != 0 || sampleValue(t, family, WorkerRunDuration+"_count", worker) != 0 {
		t.Fatal("sum or count is not zero before any observation")
	}
}

// A duration that is not a finite number would turn the sum into NaN or +Inf
// for the life of the process, and every later scrape would carry it.
func TestRunDurationIgnoresNonFiniteObservations(t *testing.T) {
	counters := NewCounters()
	counters.Observe(WorkerRunDuration, math.NaN())
	counters.Observe(WorkerRunDuration, math.Inf(1))
	counters.Observe(WorkerRunDuration, 2)
	counters.Observe("twinwright_worker_undeclared_seconds", 1)
	body, err := Collector{Counters: counters, WorkerID: "w"}.Render(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	families := mustParse(t, body)
	worker := map[string]string{"worker": "w"}
	if sum := sampleValue(t, families[WorkerRunDuration], WorkerRunDuration+"_sum", worker); sum != 2 {
		t.Fatalf("sum=%v", sum)
	}
	if count := sampleValue(t, families[WorkerRunDuration], WorkerRunDuration+"_count", worker); count != 1 {
		t.Fatalf("count=%v", count)
	}
	if _, ok := families["twinwright_worker_undeclared_seconds"]; ok {
		t.Fatal("an undeclared histogram was rendered")
	}
}

// Prometheus reads any decrease of a counter as a reset, so rate() and increase()
// jump by the whole accumulated total. A counter therefore only goes up: a
// negative delta (a duration measured across a backward clock step, say) and a
// delta that is not a finite number are dropped.
func TestCountersRefuseNegativeDeltas(t *testing.T) {
	counters := NewCounters()
	counters.Add(WorkerRunSeconds, "", 5)
	counters.Add(WorkerRunSeconds, "", -2)
	counters.Add(WorkerRunSeconds, "", math.NaN())
	counters.Add(WorkerRunSeconds, "", math.Inf(1))
	counters.Add(WorkerRunSeconds, "", math.Inf(-1))
	counters.Add(WorkerClaims, "", 1)
	counters.Add(WorkerClaims, "", -1)
	counters.Add(WorkerDispositions, "finished", 2)
	counters.Add(WorkerDispositions, "finished", -1)
	counters.Add(WorkerClaims, "", 0) // a zero delta is a valid no-op, not a refusal

	body, err := Collector{Counters: counters, WorkerID: "w"}.Render(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	families := mustParse(t, body)
	worker := map[string]string{"worker": "w"}
	if got := sampleValue(t, families[WorkerRunSeconds], WorkerRunSeconds, worker); got != 5 {
		t.Fatalf("run seconds=%v want 5: a negative or non-finite delta was applied", got)
	}
	if got := sampleValue(t, families[WorkerClaims], WorkerClaims, worker); got != 1 {
		t.Fatalf("claims=%v want 1", got)
	}
	if got := sampleValue(t, families[WorkerDispositions], WorkerDispositions, map[string]string{"disposition": "finished", "worker": "w"}); got != 2 {
		t.Fatalf("finished=%v want 2", got)
	}
}

// A histogram's sum is a counter too, so a negative observation would lower it,
// and an average-duration query built on sum and count would go wrong with it.
// The observation is dropped whole: counting it without summing it would skew the
// average the other way. Zero is a real duration and is kept.
func TestRunDurationIgnoresNegativeObservations(t *testing.T) {
	counters := NewCounters()
	counters.Observe(WorkerRunDuration, 2)
	counters.Observe(WorkerRunDuration, -2)
	counters.Observe(WorkerRunDuration, math.Inf(-1))
	counters.Observe(WorkerRunDuration, 0)
	body, err := Collector{Counters: counters, WorkerID: "w"}.Render(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	family := mustParse(t, body)[WorkerRunDuration]
	worker := map[string]string{"worker": "w"}
	if sum := sampleValue(t, family, WorkerRunDuration+"_sum", worker); sum != 2 {
		t.Fatalf("sum=%v want 2", sum)
	}
	if count := sampleValue(t, family, WorkerRunDuration+"_count", worker); count != 2 {
		t.Fatalf("count=%v want 2 (the 2s run and the 0s run)", count)
	}
	// The first bucket holds the zero observation only, so a negative one did not
	// land there.
	if first := sampleValue(t, family, WorkerRunDuration+"_bucket", map[string]string{"le": "0.05", "worker": "w"}); first != 1 {
		t.Fatalf("le=0.05 bucket=%v want 1", first)
	}
}

func TestOldestRunnableAge(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	world, err := s.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(availableAt time.Time) string {
		t.Helper()
		run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "task", "")
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Enqueue(ctx, run.ID, 10, availableAt); err != nil {
			t.Fatal(err)
		}
		return run.ID
	}
	collector := Collector{Store: s, Counters: NewCounters(), WorkerID: "w", Clock: func() time.Time { return now }}
	age := func() float64 {
		t.Helper()
		body, err := collector.Render(ctx)
		if err != nil {
			t.Fatal(err)
		}
		family := mustParse(t, body)["twinwright_queue_oldest_runnable_age_seconds"]
		if family == nil || family.kind != "gauge" {
			t.Fatalf("no gauge family\n%s", body)
		}
		return sampleValue(t, family, "twinwright_queue_oldest_runnable_age_seconds", map[string]string{})
	}

	if got := age(); got != 0 {
		t.Fatalf("empty queue age=%v want 0", got)
	}

	// The oldest entry is already leased, so it is not waiting for a worker.
	enqueue(now.Add(-300 * time.Second))
	if _, _, err = s.ClaimRun(ctx, "worker-a", time.Hour, now); err != nil {
		t.Fatal(err)
	}
	// A run in backoff is not runnable yet, however old its enqueue time is.
	enqueue(now.Add(60 * time.Second))
	if got := age(); got != 0 {
		t.Fatalf("only a leased entry and a backed-off one: age=%v want 0", got)
	}

	enqueue(now.Add(-30 * time.Second))
	if got := age(); got != 30 {
		t.Fatalf("one runnable entry available 30s ago: age=%v want 30", got)
	}
	enqueue(now.Add(-90 * time.Second))
	if got := age(); got != 90 {
		t.Fatalf("oldest of two runnable entries: age=%v want 90", got)
	}

	// Time moving on ages the entry without any write: the gauge is computed at
	// scrape time from the injected clock.
	now = now.Add(10 * time.Second)
	if got := age(); got != 100 {
		t.Fatalf("after 10s more: age=%v want 100", got)
	}
}

// oldestRunnableAgeFor renders the gauge for a store at the given instant.
func oldestRunnableAgeFor(t *testing.T, s *store.Store, now time.Time) float64 {
	t.Helper()
	body, err := Collector{Store: s, Counters: NewCounters(), WorkerID: "w", Clock: func() time.Time { return now }}.Render(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	family := mustParse(t, body)[queueOldestRunnableAge]
	return sampleValue(t, family, queueOldestRunnableAge, map[string]string{})
}

// queuedRun creates a run and enqueues it as available at availableAt.
func queuedRun(t *testing.T, s *store.Store, availableAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	world, err := s.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Enqueue(ctx, run.ID, 10, availableAt); err != nil {
		t.Fatal(err)
	}
	return run.ID
}

// A worker that dies leaves its run marked leased with a lease that then
// expires, and ClaimRun takes such a run over: it is the product's recovery path.
// The gauge has to count it, from the moment the lease expired, or an alert on it
// stays silent while a crashed fleet leaves work claimable.
func TestOldestRunnableAgeCountsAnExpiredLease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	runID := queuedRun(t, s, now.Add(-20*time.Minute))
	// worker-a claims ten minutes ago with a one minute lease and is never heard
	// from again, so the lease expired nine minutes ago.
	if _, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if got := oldestRunnableAgeFor(t, s, now.Add(-10*time.Minute+30*time.Second)); got != 0 {
		t.Fatalf("while the lease is still live: age=%v want 0", got)
	}
	// Measured from the expiry, not from the enqueue time: for twenty minutes
	// the run was not claimable for eleven of them.
	if got := oldestRunnableAgeFor(t, s, now); got != 540 {
		t.Fatalf("lease expired 9 minutes ago: age=%v want 540", got)
	}
	// The gauge's claim is true: a worker can take the run.
	lease, entry, err := s.ClaimRun(ctx, "worker-b", time.Minute, now)
	if err != nil || entry.RunID != runID || !entry.Takeover || lease.Token != 2 {
		t.Fatalf("worker-b claim: entry=%+v lease=%+v err=%v", entry, lease, err)
	}
	if got := oldestRunnableAgeFor(t, s, now); got != 0 {
		t.Fatalf("after the takeover: age=%v want 0", got)
	}
}

// An operator who re-enqueues a run that another worker is executing resets the
// queue row to runnable and leaves the live lease alone. ClaimRun refuses that
// run, so no worker can end its wait and the gauge must not report one.
func TestOldestRunnableAgeIgnoresARunHeldByALiveLease(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	runID := queuedRun(t, s, now.Add(-5*time.Minute))
	if _, _, err := s.ClaimRun(ctx, "worker-a", time.Hour, now.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(ctx, runID, 10, now.Add(-4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimRun(ctx, "worker-b", time.Minute, now); !errors.Is(err, store.ErrNoWork) {
		t.Fatalf("worker-b claim err=%v, want ErrNoWork: the premise of the test is a run no worker can claim", err)
	}
	if got := oldestRunnableAgeFor(t, s, now); got != 0 {
		t.Fatalf("a run held by a live lease: age=%v want 0", got)
	}

	// Once the holder's lease lapses the same run is claimable, and it has been
	// since the lease expired (an hour after it was taken), not since the re-enqueue.
	later := now.Add(-5*time.Minute + time.Hour + 90*time.Second)
	if got := oldestRunnableAgeFor(t, s, later); got != 90 {
		t.Fatalf("90s after the holder's lease lapsed: age=%v want 90", got)
	}
}

// The gauge may only count what ClaimRun would take. Both read the same
// predicate (store.Store.OldestClaimableSince), and this walks the states a queue
// entry can be in so that a change to one cannot leave the other behind.
func TestOldestRunnableAgeAgreesWithClaimRun(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, s *store.Store)
		want  bool // claimable
	}{
		{"empty queue", func(*testing.T, *store.Store) {}, false},
		{"runnable and available", func(t *testing.T, s *store.Store) { queuedRun(t, s, now.Add(-time.Minute)) }, true},
		{"runnable in backoff", func(t *testing.T, s *store.Store) { queuedRun(t, s, now.Add(time.Minute)) }, false},
		{"leased with a live lease", func(t *testing.T, s *store.Store) {
			queuedRun(t, s, now.Add(-time.Minute))
			if _, _, err := s.ClaimRun(ctx, "worker-a", time.Hour, now); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"leased with an expired lease", func(t *testing.T, s *store.Store) {
			queuedRun(t, s, now.Add(-time.Hour))
			if _, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, now.Add(-30*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"runnable with a live foreign lease", func(t *testing.T, s *store.Store) {
			id := queuedRun(t, s, now.Add(-time.Hour))
			if _, _, err := s.ClaimRun(ctx, "worker-a", time.Hour, now); err != nil {
				t.Fatal(err)
			}
			if err := s.Enqueue(ctx, id, 10, now.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"runnable with an expired lease", func(t *testing.T, s *store.Store) {
			id := queuedRun(t, s, now.Add(-time.Hour))
			if _, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, now.Add(-30*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := s.Enqueue(ctx, id, 10, now.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"finished", func(t *testing.T, s *store.Store) {
			queuedRun(t, s, now.Add(-time.Hour))
			lease, _, err := s.ClaimRun(ctx, "worker-a", time.Hour, now.Add(-time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if err = s.FinishRun(ctx, lease, now); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"failed", func(t *testing.T, s *store.Store) {
			queuedRun(t, s, now.Add(-time.Hour))
			lease, _, err := s.ClaimRun(ctx, "worker-a", time.Hour, now.Add(-time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if err = s.FailRun(ctx, lease, now, "boom"); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			tc.setup(t, s)
			gauge := oldestRunnableAgeFor(t, s, now)
			if (gauge > 0) != tc.want {
				t.Fatalf("gauge=%v, want it above zero: %v", gauge, tc.want)
			}
			_, _, err := s.ClaimRun(ctx, "worker-z", time.Minute, now)
			if claimable := err == nil; claimable != tc.want {
				t.Fatalf("ClaimRun err=%v, claimable=%v want %v", err, claimable, tc.want)
			}
		})
	}
}

// Every worker process with the default ID adds a row, and nothing used to remove
// one, so the last-seen series and the registered count grew by one per restart and
// an alert on last-seen age fired forever for each past process. A worker not seen
// within the window is left out of both. The count and the gauge agree on which
// workers there are.
func TestLastSeenAgeOmitsPrunedWorkers(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for id, seen := range map[string]time.Time{
		"seen-now":      now.Add(-5 * time.Second),
		"inside":        now.Add(-DefaultWorkerWindow + time.Second),
		"on-the-edge":   now.Add(-DefaultWorkerWindow),
		"just-outside":  now.Add(-DefaultWorkerWindow - time.Second),
		"long-gone":     now.Add(-72 * time.Hour),
		"clock-skewed":  now.Add(30 * time.Second),
		"older-restart": now.Add(-24 * time.Hour),
	} {
		if err := s.RegisterWorker(ctx, id, "host", seen); err != nil {
			t.Fatal(err)
		}
	}
	render := func(window time.Duration) (map[string]float64, float64, string) {
		t.Helper()
		body, err := Collector{Store: s, Counters: NewCounters(), WorkerID: "w", Clock: func() time.Time { return now }, WorkerWindow: window}.Render(ctx)
		if err != nil {
			t.Fatal(err)
		}
		families := mustParse(t, body)
		ages := map[string]float64{}
		for _, sample := range families[workerLastSeenAge].samples {
			ages[sample.labels["worker"]] = sample.value
		}
		return ages, sampleValue(t, families["twinwright_workers_registered"], "twinwright_workers_registered", map[string]string{}), body
	}

	ages, registered, body := render(0)
	want := []string{"clock-skewed", "inside", "on-the-edge", "seen-now"}
	if len(ages) != len(want) {
		t.Fatalf("last seen series = %v, want exactly %v", ages, want)
	}
	for _, id := range want {
		if _, ok := ages[id]; !ok {
			t.Fatalf("last seen series = %v, want %s in it", ages, id)
		}
	}
	if registered != float64(len(want)) {
		t.Fatalf("registered = %v, want %d: the count must agree with the series", registered, len(want))
	}
	if !strings.Contains(body, DefaultWorkerWindow.String()) {
		t.Fatalf("the HELP text does not state the window %s\n%s", DefaultWorkerWindow, body)
	}

	// A narrower window drops more, and a wider one keeps the day-old restart.
	if ages, registered, _ = render(time.Minute); len(ages) != 2 || registered != 2 {
		t.Fatalf("one minute window: series=%v registered=%v, want seen-now and clock-skewed", ages, registered)
	}
	if ages, registered, _ = render(48 * time.Hour); len(ages) != 6 || registered != 6 {
		t.Fatalf("48 hour window: series=%v registered=%v, want every worker but long-gone", ages, registered)
	}
}

// The first takeover, the first fencing rejection or the first denial would
// otherwise appear in the endpoint as a new series at 1, and increase() over a
// window that holds no earlier sample reads nothing: the first event is the one an
// alert exists for. ADR 0021 says every series is emitted even when its value is
// zero, so the kinds the ownership log can hold render at zero, and so do the
// ledger event types the metrics already single out.
func TestOwnershipEventsRenderEveryKindAtZero(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	render := func() map[string]*expoFamily {
		t.Helper()
		body, err := Collector{Store: s, Counters: NewCounters(), WorkerID: "w", Clock: func() time.Time { return now }}.Render(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return mustParse(t, body)
	}
	ownership := func(families map[string]*expoFamily, kind string) float64 {
		t.Helper()
		return sampleValue(t, families["twinwright_ownership_events"], "twinwright_ownership_events", map[string]string{"kind": kind})
	}

	families := render()
	if got, want := len(families["twinwright_ownership_events"].samples), len(store.OwnershipKinds()); got != want {
		t.Fatalf("an empty database renders %d ownership series, want %d", got, want)
	}
	for _, kind := range store.OwnershipKinds() {
		if got := ownership(families, kind); got != 0 {
			t.Fatalf("kind %q = %v on an empty database", kind, got)
		}
	}
	for _, typ := range []string{"tool.response", "retry", "authorization.denied", "chaos.injected"} {
		if got := sampleValue(t, families["twinwright_ledger_events"], "twinwright_ledger_events", map[string]string{"type": typ}); got != 0 {
			t.Fatalf("ledger type %q = %v on an empty database", typ, got)
		}
	}

	// The first rejection moves its own series from 0 to 1 and no other.
	run := queuedRun(t, s, now.Add(-time.Minute))
	if err := s.RecordFenceRejection(ctx, store.Fence{RunID: run, Owner: "worker-a", Token: 1}, now, "test"); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, run, "retry", map[string]any{"attempt": 1}); err != nil {
		t.Fatal(err)
	}
	families = render()
	for _, kind := range store.OwnershipKinds() {
		want := 0.0
		if kind == store.OwnershipFenced {
			want = 1
		}
		if got := ownership(families, kind); got != want {
			t.Fatalf("after one fencing rejection kind %q = %v want %v", kind, got, want)
		}
	}
	if got := sampleValue(t, families["twinwright_ledger_events"], "twinwright_ledger_events", map[string]string{"type": "retry"}); got != 1 {
		t.Fatalf("ledger type retry = %v after one retry event", got)
	}

	// A kind this build does not know, written by another build, is still shown.
	if err := s.AppendOwnership(ctx, run, "lease.future-kind", "worker-a", 1, now, nil); err != nil {
		t.Fatal(err)
	}
	if got := ownership(render(), "lease.future-kind"); got != 1 {
		t.Fatalf("an unknown kind = %v, want it counted", got)
	}
}

func TestWorkerLastSeenAge(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for id, seen := range map[string]time.Time{
		"worker-a": now.Add(-45 * time.Second),
		"worker-b": now.Add(-5 * time.Second),
		// A stamp from a host whose clock runs ahead is not a negative age.
		"worker-skew": now.Add(10 * time.Second),
	} {
		if err := s.RegisterWorker(ctx, id, "host", seen); err != nil {
			t.Fatal(err)
		}
	}
	body, err := Collector{Store: s, Counters: NewCounters(), WorkerID: "worker-a", Clock: func() time.Time { return now }}.Render(ctx)
	if err != nil {
		t.Fatal(err)
	}
	family := mustParse(t, body)["twinwright_worker_last_seen_age_seconds"]
	if family == nil || family.kind != "gauge" {
		t.Fatalf("no gauge family\n%s", body)
	}
	for id, want := range map[string]float64{"worker-a": 45, "worker-b": 5, "worker-skew": 0} {
		if got := sampleValue(t, family, family.name, map[string]string{"worker": id}); got != want {
			t.Errorf("%s last seen age=%v want %v", id, got, want)
		}
	}
	if len(family.samples) != 3 {
		t.Fatalf("samples=%v want one per registered worker", family.samples)
	}
}

func TestExpositionIsWellFormed(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	world, err := s.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Enqueue(ctx, run.ID, 10, now.Add(-20*time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, eventType := range []string{"tool.response", "retry", "authorization.denied", "chaos.injected"} {
		if err = s.Append(ctx, run.ID, eventType, map[string]any{"call_id": "c1"}); err != nil {
			t.Fatal(err)
		}
	}
	// The lease is acquired long ago and has expired. A live lease on a runnable
	// entry would make the entry unclaimable, and this test reads the age of one a
	// worker can claim.
	if _, err = s.AcquireRunLease(ctx, run.ID, "worker-a", time.Minute, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Worker IDs are operator input and other workers' IDs are rendered too, so
	// quote, backslash and newline must survive as label escapes.
	const odd = "odd\"worker\\id\nline2"
	if err = s.RegisterWorker(ctx, odd, "host", now.Add(-3*time.Second)); err != nil {
		t.Fatal(err)
	}

	counters := NewCounters()
	counters.Add(WorkerClaims, "", 1)
	counters.Add(WorkerDispositions, "finished", 1)
	counters.Observe(WorkerRunDuration, 0.4)
	body, err := Collector{Store: s, Counters: counters, WorkerID: odd, Clock: func() time.Time { return now }}.Render(ctx)
	if err != nil {
		t.Fatal(err)
	}
	families := mustParse(t, body)

	for name, kind := range map[string]string{
		WorkerClaims: "counter", WorkerDispositions: "counter", WorkerLeaseRenewals: "counter",
		WorkerFencingRejections: "counter", WorkerTakeovers: "counter", WorkerRunSeconds: "counter",
		WorkerRunDuration:                              "histogram",
		"twinwright_queue_entries":                     "gauge",
		"twinwright_queue_oldest_runnable_age_seconds": "gauge",
		"twinwright_runs":                              "gauge",
		"twinwright_ledger_events":                     "gauge",
		"twinwright_tool_calls":                        "gauge",
		"twinwright_retries":                           "gauge",
		"twinwright_authorization_denials":             "gauge",
		"twinwright_chaos_injections":                  "gauge",
		"twinwright_ownership_events":                  "gauge",
		"twinwright_workers_registered":                "gauge",
		"twinwright_worker_last_seen_age_seconds":      "gauge",
		"twinwright_scrape_timestamp_seconds":          "gauge",
	} {
		family := families[name]
		if family == nil {
			t.Errorf("family %s is missing", name)
			continue
		}
		if family.kind != kind {
			t.Errorf("%s is a %s, want %s", name, family.kind, kind)
		}
	}
	if got := sampleValue(t, families["twinwright_worker_claims_total"], "twinwright_worker_claims_total", map[string]string{"worker": odd}); got != 1 {
		t.Fatalf("a worker ID needing escapes did not round-trip through the exposition: claims=%v", got)
	}
	if got := sampleValue(t, families["twinwright_worker_last_seen_age_seconds"], "twinwright_worker_last_seen_age_seconds", map[string]string{"worker": odd}); got != 3 {
		t.Fatalf("last seen age=%v want 3", got)
	}
	if got := sampleValue(t, families["twinwright_queue_oldest_runnable_age_seconds"], "twinwright_queue_oldest_runnable_age_seconds", map[string]string{}); got != 20 {
		t.Fatalf("oldest runnable age=%v want 20", got)
	}
}
