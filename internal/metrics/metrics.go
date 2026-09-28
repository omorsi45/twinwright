// Package metrics exposes Twinwright runtime counters in the Prometheus text
// format.
//
// Two sources feed it, and they are deliberately different in kind.
//
// Counters that belong to THIS PROCESS - claims, dispositions, fencing
// rejections, lease renewals - are accumulated in memory by the worker as it
// runs, because they describe events, and an event that has already happened
// cannot be recovered from current state.
//
// Gauges that belong to the SYSTEM - queue depth, run statuses, tool calls,
// retries, authorization denials, chaos injections - are read from the database
// at scrape time. They are derived from the durable ledger, which keeps the
// ledger the single source of truth rather than introducing a second set of
// numbers that can disagree with it.
//
// There is no Prometheus client dependency: the text format is a documented,
// stable, line-oriented contract, and a scraper only needs correct bytes.
package metrics

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"twinwright/internal/store"
)

// Counters holds in-process event counts.
type Counters struct {
	mu     sync.Mutex
	values map[string]map[string]float64 // metric -> label value -> count
}

// NewCounters returns an empty counter set.
func NewCounters() *Counters {
	return &Counters{values: map[string]map[string]float64{}}
}

// Add increments a counter. label may be empty for an unlabelled metric.
func (c *Counters) Add(metric, label string, delta float64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values[metric] == nil {
		c.values[metric] = map[string]float64{}
	}
	c.values[metric][label] += delta
}

// snapshot copies the counters for rendering. Nil-safe, like Add: a collector
// configured without counters still renders the system gauges.
func (c *Counters) snapshot() map[string]map[string]float64 {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]map[string]float64, len(c.values))
	for metric, labels := range c.values {
		copied := make(map[string]float64, len(labels))
		for label, value := range labels {
			copied[label] = value
		}
		out[metric] = copied
	}
	return out
}

// Counter metric names. Keeping them as constants stops a typo from silently
// creating a second series that nobody graphs.
const (
	WorkerClaims            = "twinwright_worker_claims_total"
	WorkerDispositions      = "twinwright_worker_claim_disposition_total"
	WorkerLeaseRenewals     = "twinwright_worker_lease_renewals_total"
	WorkerFencingRejections = "twinwright_worker_fencing_rejections_total"
	WorkerTakeovers         = "twinwright_worker_takeovers_total"
	WorkerRunSeconds        = "twinwright_worker_run_seconds_total"
)

// Collector renders the metrics endpoint.
type Collector struct {
	Store    *store.Store
	Counters *Counters
	// WorkerID labels this process's own counters.
	WorkerID string
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

	writeHelp(&b, WorkerClaims, "counter", "Runs claimed by this worker.")
	writeHelp(&b, WorkerDispositions, "counter", "Claims by how they ended.")
	writeHelp(&b, WorkerLeaseRenewals, "counter", "Successful lease renewals by this worker.")
	writeHelp(&b, WorkerFencingRejections, "counter", "Commits refused because this worker no longer owned the run.")
	writeHelp(&b, WorkerTakeovers, "counter", "Runs this worker took over from a previous owner.")
	writeHelp(&b, WorkerRunSeconds, "counter", "Seconds spent executing claimed runs.")

	snapshot := c.Counters.snapshot()
	for _, metric := range sortedKeys(snapshot) {
		labels := snapshot[metric]
		for _, label := range sortedKeys(labels) {
			pairs := map[string]string{"worker": c.WorkerID}
			if label != "" {
				pairs["disposition"] = label
			}
			writeSample(&b, metric, pairs, labels[label])
		}
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

	writeHelp(&b, "twinwright_scrape_timestamp_seconds", "gauge", "When this document was produced.")
	writeSample(&b, "twinwright_scrape_timestamp_seconds", nil, float64(time.Now().UTC().Unix()))
	return b.String(), nil
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
			fmt.Fprintf(b, "%s=%q", key, escapeLabel(labels[key]))
		}
		b.WriteString("}")
	}
	b.WriteString(" ")
	b.WriteString(strconv.FormatFloat(value, 'f', -1, 64))
	b.WriteString("\n")
}

func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
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
