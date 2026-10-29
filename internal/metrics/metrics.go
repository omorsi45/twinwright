// Package metrics exposes Twinwright runtime counters in the Prometheus text
// format.
//
// Two sources feed it, and they are deliberately different in kind.
//
// Counters and the run-duration histogram that belong to THIS PROCESS -
// claims, dispositions, fencing rejections, lease renewals, how long runs took
// - are accumulated in memory by the worker as it runs, because they describe
// events, and an event that has already happened cannot be recovered from
// current state.
//
// Gauges that belong to the SYSTEM - queue depth, how long the oldest runnable
// entry has waited, run statuses, tool calls, retries, authorization denials,
// chaos injections, when each worker was last seen - are read from the
// database at scrape time. They are derived from the durable ledger and the
// coordination tables, which keeps those the single source of truth rather
// than introducing a second set of numbers that can disagree with them. Every
// worker serves the same fleet-wide gauges for the database it points at.
//
// Every family is written contiguously (HELP, TYPE, then all of its samples)
// before the next one starts, which is the shape the text format requires and
// scrapers rely on.
//
// There is no Prometheus client dependency: the text format is a documented,
// stable, line-oriented contract, and a scraper only needs correct bytes.
package metrics

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"twinwright/internal/store"
)

// Counter and histogram metric names. Keeping them as constants stops a typo
// from silently creating a second series that nobody graphs.
const (
	WorkerClaims            = "twinwright_worker_claims_total"
	WorkerDispositions      = "twinwright_worker_claim_disposition_total"
	WorkerLeaseRenewals     = "twinwright_worker_lease_renewals_total"
	WorkerFencingRejections = "twinwright_worker_fencing_rejections_total"
	WorkerTakeovers         = "twinwright_worker_takeovers_total"
	WorkerRunSeconds        = "twinwright_worker_run_seconds_total"
	WorkerRunDuration       = "twinwright_worker_run_duration_seconds"
)

const (
	queueOldestRunnableAge = "twinwright_queue_oldest_runnable_age_seconds"
	workerLastSeenAge      = "twinwright_worker_last_seen_age_seconds"
	dispositionLabel       = "disposition"
)

// dispositions are the values of the disposition label. The worker package owns
// the dispositions and cannot be imported from here without a cycle through the
// command that wires the two, so a test in cmd/twinwright keeps this list equal
// to the worker's constants.
var dispositions = []string{"failed", "fenced", "finished", "requeued"}

// DispositionLabels returns the disposition label values that are rendered at
// zero before the first claim, in sorted order.
func DispositionLabels() []string {
	return append([]string(nil), dispositions...)
}

// counterSpec declares one counter family. The label name is declared here, per
// metric, rather than inferred from whatever label value an Add happens to
// pass, which is how a claims counter once came to carry a disposition label.
type counterSpec struct {
	name string
	help string
	// label names the metric's one label besides worker; empty means none.
	label string
	// values are the label values rendered at zero before the first Add, so a
	// series exists from the first scrape and rate() has a sample to start from.
	values []string
}

var counterSpecs = []counterSpec{
	{name: WorkerClaims, help: "Runs claimed by this worker."},
	{name: WorkerDispositions, help: "Claims by how they ended.", label: dispositionLabel, values: dispositions},
	{name: WorkerLeaseRenewals, help: "Successful lease renewals by this worker."},
	{name: WorkerFencingRejections, help: "Commits refused because this worker no longer owned the run."},
	{name: WorkerTakeovers, help: "Runs this worker took over from a previous owner."},
	{name: WorkerRunSeconds, help: "Seconds spent executing claimed runs."},
}

// histogramSpec declares one histogram family. Its only label is worker.
type histogramSpec struct {
	name string
	help string
	// bounds are the inclusive upper bounds of the finite buckets, ascending.
	// The +Inf bucket is implicit.
	bounds []float64
}

var histogramSpecs = []histogramSpec{
	{
		name:   WorkerRunDuration,
		help:   "Seconds from claim to the end of the claim, for every disposition.",
		bounds: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
	},
}

func counterSpecFor(metric string) (counterSpec, bool) {
	for _, spec := range counterSpecs {
		if spec.name == metric {
			return spec, true
		}
	}
	return counterSpec{}, false
}

func histogramSpecFor(metric string) (histogramSpec, bool) {
	for _, spec := range histogramSpecs {
		if spec.name == metric {
			return spec, true
		}
	}
	return histogramSpec{}, false
}

// histogram holds the observations of one histogram family.
type histogram struct {
	// buckets counts observations per bucket, not cumulatively. Slot i is the
	// bucket with bounds[i] as its upper bound; the last slot is +Inf.
	buckets []uint64
	sum     float64
	count   uint64
}

// bucket returns the count in slot i, zero for a histogram never observed.
func (h histogram) bucket(i int) uint64 {
	if i < len(h.buckets) {
		return h.buckets[i]
	}
	return 0
}

// Counters holds in-process event counts and duration histograms.
type Counters struct {
	mu         sync.Mutex
	values     map[string]map[string]float64 // counter -> label value -> count
	histograms map[string]*histogram
}

// NewCounters returns an empty counter set.
func NewCounters() *Counters {
	return &Counters{values: map[string]map[string]float64{}, histograms: map[string]*histogram{}}
}

// Add increments a declared counter.
//
// label selects the series of a labelled counter (WorkerDispositions) and is
// required for it; an unlabelled counter ignores it, so the delta still lands
// on its one series. An undeclared metric, or a labelled counter given no
// label, is dropped: Render writes only declared families, which is what keeps
// the document well formed.
func (c *Counters) Add(metric, label string, delta float64) {
	if c == nil {
		return
	}
	spec, ok := counterSpecFor(metric)
	if !ok {
		return
	}
	if spec.label == "" {
		label = ""
	} else if label == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values[metric] == nil {
		c.values[metric] = map[string]float64{}
	}
	c.values[metric][label] += delta
}

// Observe records one value, in seconds, against a declared histogram. An
// undeclared metric is dropped. A value that is not a finite number is dropped
// too: one NaN or infinity would poison the sum for the life of the process.
func (c *Counters) Observe(metric string, value float64) {
	if c == nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	spec, ok := histogramSpecFor(metric)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.histograms[metric]
	if h == nil {
		h = &histogram{buckets: make([]uint64, len(spec.bounds)+1)}
		c.histograms[metric] = h
	}
	// The first bound that is >= value, so an upper bound is inclusive; a value
	// above every bound lands in the +Inf slot.
	h.buckets[sort.SearchFloat64s(spec.bounds, value)]++
	h.sum += value
	h.count++
}

// snapshot is a consistent copy of the counters for rendering.
type snapshot struct {
	counters   map[string]map[string]float64
	histograms map[string]histogram
}

// snapshot copies the counters. Nil-safe, like Add: a collector configured
// without counters still renders every declared family at zero, and the system
// gauges.
func (c *Counters) snapshot() snapshot {
	out := snapshot{counters: map[string]map[string]float64{}, histograms: map[string]histogram{}}
	if c == nil {
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for metric, labels := range c.values {
		copied := make(map[string]float64, len(labels))
		for label, value := range labels {
			copied[label] = value
		}
		out.counters[metric] = copied
	}
	for metric, h := range c.histograms {
		out.histograms[metric] = histogram{buckets: append([]uint64(nil), h.buckets...), sum: h.sum, count: h.count}
	}
	return out
}

// Collector renders the metrics endpoint.
type Collector struct {
	Store    *store.Store
	Counters *Counters
	// WorkerID labels this process's own counters.
	WorkerID string
	// Clock is for deterministic tests; nil means time.Now.
	Clock store.Clock
}

// Handler serves the Prometheus text exposition format.
func (c Collector) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := c.Render(r.Context())
		if err != nil {
			// A scrape must not return half a document: a truncated body would
			// be parsed as a set of zeroed series, which reads as an outage.
			http.Error(w, "# metrics unavailable: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(body))
	})
}

// Render produces the exposition document.
func (c Collector) Render(ctx context.Context) (string, error) {
	var b strings.Builder
	now := c.Clock.Now()

	snap := c.Counters.snapshot()
	for _, spec := range counterSpecs {
		writeCounter(&b, spec, c.WorkerID, snap.counters[spec.name])
	}
	for _, spec := range histogramSpecs {
		writeHistogram(&b, spec, c.WorkerID, snap.histograms[spec.name])
	}

	if c.Store == nil {
		return b.String(), nil
	}

	depth, err := c.Store.QueueDepth(ctx)
	if err != nil {
		return "", fmt.Errorf("queue depth: %w", err)
	}
	writeHelp(&b, "twinwright_queue_entries", "gauge", "Work queue entries by state.")
	for _, state := range []string{store.QueueRunnable, store.QueueLeased, store.QueueFinished, store.QueueFailed} {
		writeSample(&b, "twinwright_queue_entries", map[string]string{"state": state}, float64(depth[state]))
	}

	oldest, err := oldestRunnableAge(ctx, c.Store, now)
	if err != nil {
		return "", fmt.Errorf("oldest runnable entry: %w", err)
	}
	writeHelp(&b, queueOldestRunnableAge, "gauge",
		"Seconds the longest-waiting entry a worker could claim right now has been claimable: a runnable entry counts from when it became available, an entry whose lease expired from when the lease expired; 0 when none. An entry in backoff, and a run another worker's live lease holds, do not count.")
	writeSample(&b, queueOldestRunnableAge, nil, oldest)

	runs, err := countBy(ctx, c.Store, "SELECT status, count(*) FROM runs GROUP BY status")
	if err != nil {
		return "", fmt.Errorf("run statuses: %w", err)
	}
	writeHelp(&b, "twinwright_runs", "gauge", "Runs by status, from the durable ledger.")
	for _, status := range []string{"running", "paused", "completed", "failed"} {
		writeSample(&b, "twinwright_runs", map[string]string{"status": status}, float64(runs[status]))
	}

	// Event-derived gauges. Each one is a count of ledger events, so it cannot
	// drift from the ledger the way a separately maintained counter could.
	events, err := countBy(ctx, c.Store, "SELECT type, count(*) FROM events GROUP BY type")
	if err != nil {
		return "", fmt.Errorf("event counts: %w", err)
	}
	writeHelp(&b, "twinwright_ledger_events", "gauge", "Ledger events by type.")
	for _, typ := range sortedKeys(events) {
		writeSample(&b, "twinwright_ledger_events", map[string]string{"type": typ}, float64(events[typ]))
	}

	writeHelp(&b, "twinwright_tool_calls", "gauge", "Tool responses recorded in the ledger.")
	writeSample(&b, "twinwright_tool_calls", nil, float64(events["tool.response"]))
	writeHelp(&b, "twinwright_retries", "gauge", "Retry events recorded in the ledger.")
	writeSample(&b, "twinwright_retries", nil, float64(events["retry"]))
	writeHelp(&b, "twinwright_authorization_denials", "gauge", "Authorization denials recorded in the ledger.")
	writeSample(&b, "twinwright_authorization_denials", nil, float64(events["authorization.denied"]))
	writeHelp(&b, "twinwright_chaos_injections", "gauge", "Deterministic faults injected, recorded in the ledger.")
	writeSample(&b, "twinwright_chaos_injections", nil, float64(events["chaos.injected"]))

	ownership, err := countBy(ctx, c.Store, "SELECT kind, count(*) FROM run_ownership_log GROUP BY kind")
	if err != nil {
		return "", fmt.Errorf("ownership counts: %w", err)
	}
	writeHelp(&b, "twinwright_ownership_events", "gauge", "Ownership transitions across the fleet.")
	for _, kind := range sortedKeys(ownership) {
		writeSample(&b, "twinwright_ownership_events", map[string]string{"kind": kind}, float64(ownership[kind]))
	}

	workers, err := c.Store.Workers(ctx)
	if err != nil {
		return "", fmt.Errorf("workers: %w", err)
	}
	writeHelp(&b, "twinwright_workers_registered", "gauge", "Workers that have registered against this database.")
	writeSample(&b, "twinwright_workers_registered", nil, float64(len(workers)))

	// A worker's stamp moves when it registers, claims a run or renews a lease.
	// It does not move on an idle poll, so an idle worker's age keeps growing.
	sort.Slice(workers, func(i, j int) bool { return workers[i].ID < workers[j].ID })
	writeHelp(&b, workerLastSeenAge, "gauge",
		"Seconds since each registered worker last registered, claimed a run or renewed a lease.")
	for _, worker := range workers {
		age, err := ageSeconds(worker.LastSeenAt, now)
		if err != nil {
			return "", fmt.Errorf("worker %q last seen: %w", worker.ID, err)
		}
		writeSample(&b, workerLastSeenAge, map[string]string{"worker": worker.ID}, age)
	}

	writeHelp(&b, "twinwright_scrape_timestamp_seconds", "gauge", "When this document was produced.")
	writeSample(&b, "twinwright_scrape_timestamp_seconds", nil, float64(now.UTC().Unix()))
	return b.String(), nil
}

// writeCounter writes one counter family. Every declared series is written even
// when it has never been incremented.
func writeCounter(b *strings.Builder, spec counterSpec, worker string, values map[string]float64) {
	writeHelp(b, spec.name, "counter", spec.help)
	if spec.label == "" {
		writeSample(b, spec.name, map[string]string{"worker": worker}, values[""])
		return
	}
	// The declared values are written at zero, and so is any other value that
	// was counted, so a label value added later is never dropped from the output.
	seen := map[string]bool{}
	for _, value := range spec.values {
		seen[value] = true
	}
	for value := range values {
		seen[value] = true
	}
	for _, value := range sortedKeys(seen) {
		writeSample(b, spec.name, map[string]string{"worker": worker, spec.label: value}, values[value])
	}
}

// writeHistogram writes one histogram family: cumulative buckets ending in
// +Inf, then the sum and the count. A histogram never observed still renders
// every bucket at zero.
func writeHistogram(b *strings.Builder, spec histogramSpec, worker string, h histogram) {
	writeHelp(b, spec.name, "histogram", spec.help)
	var cumulative uint64
	for i, bound := range spec.bounds {
		cumulative += h.bucket(i)
		writeSample(b, spec.name+"_bucket",
			map[string]string{"le": strconv.FormatFloat(bound, 'f', -1, 64), "worker": worker}, float64(cumulative))
	}
	cumulative += h.bucket(len(spec.bounds))
	writeSample(b, spec.name+"_bucket", map[string]string{"le": "+Inf", "worker": worker}, float64(cumulative))
	writeSample(b, spec.name+"_sum", map[string]string{"worker": worker}, h.sum)
	writeSample(b, spec.name+"_count", map[string]string{"worker": worker}, float64(h.count))
}

// oldestRunnableAge is how long the entry that has been claimable longest has
// been claimable, 0 when no worker could claim anything. The store decides what
// is claimable, with the predicate ClaimRun selects by, so the gauge answers for
// the entries a worker could take right now: a runnable entry whose backoff has
// passed, and an entry a crashed worker left behind once its lease expired. A
// backed-off entry and a run another worker's live lease still holds are not
// waiting for a worker, so they do not count.
func oldestRunnableAge(ctx context.Context, s *store.Store, now time.Time) (float64, error) {
	since, found, err := s.OldestClaimableSince(ctx, now)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	return ageSeconds(since, now)
}

// ageSeconds is how long before now a coordination timestamp was. A stamp ahead
// of now comes from a host whose clock runs ahead of this one; that is not a
// negative age, so it reads as 0.
func ageSeconds(stamp string, now time.Time) (float64, error) {
	at, err := time.Parse(store.CoordinationLayout, stamp)
	if err != nil {
		return 0, fmt.Errorf("parse coordination time %q: %w", stamp, err)
	}
	if age := now.Sub(at).Seconds(); age > 0 {
		return age, nil
	}
	return 0, nil
}

func countBy(ctx context.Context, s *store.Store, query string) (map[string]int, error) {
	rows, err := s.DB.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var key string
		var count int
		if err = rows.Scan(&key, &count); err != nil {
			return nil, err
		}
		out[key] = count
	}
	return out, rows.Err()
}

func writeHelp(b *strings.Builder, name, kind, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

func writeSample(b *strings.Builder, name string, labels map[string]string, value float64) {
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteString("{")
		first := true
		for _, key := range sortedKeys(labels) {
			if !first {
				b.WriteString(",")
			}
			first = false
			b.WriteString(key)
			b.WriteString(`="`)
			b.WriteString(escapeLabel(labels[key]))
			b.WriteString(`"`)
		}
		b.WriteString("}")
	}
	b.WriteString(" ")
	b.WriteString(strconv.FormatFloat(value, 'f', -1, 64))
	b.WriteString("\n")
}

// escapeLabel applies the three escapes the text format defines for a label
// value: backslash, double quote and newline. The value is written between
// quotes by the caller, so it must not be passed through %q as well: that
// escapes a second time, and a worker ID containing a backslash would come out
// doubled.
func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return value
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Serve runs the metrics endpoint until ctx is cancelled.
//
// It binds exactly where the caller asks. Callers pass a loopback address by
// default: these counters describe internal runtime state and there is no
// authentication on this endpoint, so exposing it on a public interface would
// hand queue and run topology to anyone who asked. The docstring says so
// because the code cannot enforce an operator's choice of address.
func Serve(ctx context.Context, address string, collector Collector) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", collector.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	server := &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-done:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
