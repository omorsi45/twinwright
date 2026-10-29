package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"twinwright/internal/agent"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/metrics"
	"twinwright/internal/store"
	"twinwright/internal/trace"
	"twinwright/internal/worker"
)

// Distributed-mode commands.
//
// The single-node commands still execute a run inline, which is what keeps the
// deterministic examples and replay fixtures simple. Distributed mode splits
// that in two: `run --enqueue` creates a run and puts it on the queue, and
// `worker` claims queued runs under a fenced lease and executes them. Both
// halves talk to the same database, which may be SQLite for a local try or
// PostgreSQL for a real fleet.

// enqueueCommand puts an existing run on the work queue.
func enqueueCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: twinwright enqueue <run-id> [--db dsn] [--steps n]")
	}
	runID := args[1]
	fs := flag.NewFlagSet("enqueue", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dbPath := fs.String("db", "twinwright.db", "SQLite path or postgres:// DSN")
	steps := fs.Int("steps", 20, "maximum model turns per claim")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	s, err := store.OpenDSN(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer s.Close()
	if err = s.Enqueue(ctx, runID, *steps, time.Now().UTC()); err != nil {
		return err
	}
	entry, err := s.QueueEntryFor(ctx, runID)
	if err != nil {
		return err
	}
	return writeJSON(out, map[string]any{
		"run_id":   entry.RunID,
		"state":    entry.State,
		"steps":    entry.MaxSteps,
		"delivery": "at_least_once",
	})
}

// openForReading opens the database a read-only command examines. It uses the
// read-only open, so it never creates a database for a mistyped path and never
// migrates one, and then checks that the schema has what the command reads.
// The docs and AGENTS.md promise that inspection never changes the source; an
// operator who watches a rolling upgrade with `queue` or `doctor` depends on it.
func openForReading(ctx context.Context, dsn string, minSchema int, feature string) (*store.Store, error) {
	s, err := store.OpenReadOnlyDSN(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err = s.RequireSchema(ctx, minSchema, feature); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// queueCommand reports queue depth, the registered fleet and per-run ownership.
// It only reads: see openForReading.
func queueCommand(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("queue", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dbPath := fs.String("db", "twinwright.db", "SQLite path or postgres:// DSN")
	runID := fs.String("run", "", "show one run's queue entry and ownership history")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	s, err := openForReading(ctx, *dbPath, store.SchemaVersionQueue, "the work queue")
	if err != nil {
		return err
	}
	defer s.Close()

	if *runID != "" {
		entry, err := s.QueueEntryFor(ctx, *runID)
		if err != nil {
			return err
		}
		history, err := s.Ownership(ctx, *runID)
		if err != nil {
			return err
		}
		payload := map[string]any{"entry": entry, "ownership": history}
		if lease, err := s.RunLease(ctx, *runID); err == nil {
			payload["lease"] = lease
		}
		return writeJSON(out, payload)
	}

	depth, err := s.QueueDepth(ctx)
	if err != nil {
		return err
	}
	workers, err := s.Workers(ctx)
	if err != nil {
		return err
	}
	return writeJSON(out, map[string]any{
		"depth":    depth,
		"workers":  workers,
		"delivery": "at_least_once",
		"backend":  string(s.Dialect),
	})
}

// executeLocally runs a run from the CLI under the same fenced lease a worker
// holds, so `run` and `resume` cannot execute alongside a fleet worker on the
// same run. Ctrl-C and SIGTERM cancel the run and still release the lease.
func executeLocally(ctx context.Context, s *store.Store, manifest compiler.Manifest, provider agent.Provider, runID string, steps int) (store.Run, error) {
	signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// NotifyContext keeps swallowing signals until stop is called, so a second
	// Ctrl-C during a slow release would do nothing. Stopping as soon as the
	// first signal arrives hands later signals back to the default handler,
	// which ends the process.
	context.AfterFunc(signalCtx, stop)
	return worker.ExecuteLocal(signalCtx, s, runID, worker.Config{}, func(runCtx context.Context, fence store.Fence) (store.Run, error) {
		runner := agent.Runner{
			Store:    s,
			Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &fence},
			Manifest: manifest,
			Provider: provider,
			Fence:    &fence,
		}
		return runner.Execute(runCtx, runID, steps)
	})
}

// localRunError words an ExecuteLocal error for the CLI so it says what
// happened to the run. Every wording wraps err, so errors.Is still sees the
// cause.
//
//   - A run whose work is durable but whose lease was not released is reported
//     as such: calling it failed would mislead an operator into retrying
//     finished work.
//   - A lost lease is a failure even though it cancels the run's context, so it
//     is matched before cancellation.
//   - A refusal because another owner holds the lease means the run never
//     started.
//   - Cancellation is Ctrl-C or SIGTERM, which leaves a run that resume picks up.
func localRunError(runID string, err error) error {
	switch {
	case errors.Is(err, worker.ErrLeaseNotReleased):
		return err
	case errors.Is(err, store.ErrFenced):
		return fmt.Errorf("run %s failed: %w", runID, err)
	case errors.Is(err, store.ErrLeaseHeld):
		return fmt.Errorf("run %s not started: %w", runID, err)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("run %s interrupted; resume it with `twinwright resume %s`: %w", runID, runID, err)
	}
	return fmt.Errorf("run %s failed: %w", runID, err)
}

// workerCommand runs a worker process that claims and executes queued runs.
func workerCommand(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	manifestPath := fs.String("manifest", "twinwright.manifest.json", "compiled manifest")
	dbPath := fs.String("db", "twinwright.db", "SQLite path or postgres:// DSN")
	id := fs.String("id", "", "worker ID (defaults to host-pid)")
	leaseTTL := fs.Duration("lease-ttl", worker.DefaultLeaseTTL, "how long a claim is valid without renewal")
	poll := fs.Duration("poll", worker.DefaultPollInterval, "idle wait between empty claims")
	backoff := fs.Duration("backoff", worker.DefaultBackoff, "delay before a failed run is retried")
	attempts := fs.Int("max-attempts", worker.DefaultMaxAttempts, "attempts before a run is marked failed")
	maxRuns := fs.Int("max-runs", 0, "exit after this many claims (0 means serve until interrupted)")
	drain := fs.Bool("drain", false, "exit as soon as the queue is empty instead of polling")
	metricsAddr := fs.String("metrics-addr", "", "serve Prometheus metrics on this address, for example 127.0.0.1:9095 (unauthenticated: bind loopback)")
	// Unlike the trace command, the endpoint does not default to
	// OTEL_EXPORTER_OTLP_ENDPOINT: a worker that exports every completed run's
	// trace is new behaviour, so it must be asked for with the flag.
	otlpEndpoint := fs.String("otlp-endpoint", "", "POST each completed run's trace to this OpenTelemetry collector (empty disables export)")
	otlpHeader := fs.String("otlp-header", "", "extra request header for the collector, as Name: value; with none, OTEL_EXPORTER_OTLP_HEADERS (name=value,...) is used, which keeps a credential out of the process listing")
	providerCfg := providerFlags(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	// Checked before any file or database is touched, like a missing manifest.
	var otlpHeaders map[string]string
	if *otlpEndpoint != "" {
		headers, err := resolveOTLPHeaders(*otlpHeader)
		if err != nil {
			return err
		}
		otlpHeaders = headers
	}
	manifest, err := readManifest(*manifestPath)
	if err != nil {
		return err
	}
	s, err := store.OpenDSN(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	// The provider is chosen per run from the run's own record, so one worker
	// serves scripted fixtures and live-provider runs from the same queue.
	execute := func(runCtx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		run, err := s.Run(runCtx, lease.RunID)
		if err != nil {
			return store.Run{}, err
		}
		provider, err := selectProvider(providerCfg.forRun(run.Provider, run.Model, run.Scenario))
		if err != nil {
			return store.Run{}, err
		}
		provider = agent.WithManifest(provider, manifest)
		fence := lease.Fence()
		runner := agent.Runner{
			Store:    s,
			Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &fence},
			Manifest: manifest,
			Provider: provider,
			Fence:    &fence,
		}
		return runner.Execute(runCtx, lease.RunID, entry.MaxSteps)
	}

	w := worker.New(s, execute, worker.Config{
		ID:           *id,
		LeaseTTL:     *leaseTTL,
		PollInterval: *poll,
		Backoff:      *backoff,
		MaxAttempts:  *attempts,
		ExportTrace:  traceExporter(s, *otlpEndpoint, otlpHeaders),
	})

	// Per-process event counters. Queue depth, run statuses and the
	// ledger-derived gauges are read from the database at scrape time instead,
	// so they cannot drift from the ledger.
	counters := metrics.NewCounters()
	instrumentWorker(w, counters)
	// A run's trace is exported behind the claim loop, so the last one may still be
	// on its way when the command ends. This runs before the store is closed, which
	// the export reads from.
	defer w.Close()

	// Ctrl-C and SIGTERM cancel the context. A run in flight is not committed
	// half way: ClaimAndRun hands its claim back (runnable at once, the delivery
	// it counted given back) and returns, and the worker then stops. Only a worker
	// that is killed leaves its lease to lapse, which is the crash path.
	signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *metricsAddr != "" {
		collector := metrics.Collector{Store: s, Counters: counters, WorkerID: w.ID()}
		go func() {
			if err := metrics.Serve(signalCtx, *metricsAddr, collector); err != nil {
				fmt.Fprintf(os.Stderr, "twinwright: metrics endpoint stopped: %v\n", err)
			}
		}()
	}

	if *maxRuns == 0 && !*drain {
		if err = w.Serve(signalCtx); err != nil {
			return err
		}
		return writeJSON(out, map[string]any{"worker": w.ID(), "stopped": true})
	}

	if err = w.Register(signalCtx); err != nil {
		return err
	}
	var outcomes []worker.Outcome
	// A stop request (Ctrl-C or SIGTERM) ends the loop cleanly, like Serve: the
	// claim in flight has already been handed back by ClaimAndRun, so there is
	// nothing to report as an error. The summary says it stopped.
	stopped := false
claims:
	for *maxRuns == 0 || len(outcomes) < *maxRuns {
		outcome, err := w.ClaimAndRun(signalCtx)
		switch {
		case errors.Is(err, store.ErrNoWork):
			if *drain {
				break claims
			}
			select {
			case <-signalCtx.Done():
			case <-time.After(*poll):
				continue claims
			}
		case err != nil:
			if signalCtx.Err() == nil || !errors.Is(err, context.Canceled) {
				return err
			}
		default:
			outcomes = append(outcomes, outcome)
		}
		if signalCtx.Err() != nil {
			stopped = true
			break claims
		}
	}
	summary := map[string]any{
		"worker":   w.ID(),
		"claims":   len(outcomes),
		"outcomes": outcomes,
		"delivery": "at_least_once",
		"backend":  string(s.Dialect),
	}
	if stopped {
		summary["stopped"] = true
	}
	return writeJSON(out, summary)
}

// instrumentWorker connects a worker's events to the in-process counters the
// metrics endpoint serves: every claim is counted and timed whatever its
// disposition, and every successful lease renewal is counted.
func instrumentWorker(w *worker.Worker, counters *metrics.Counters) {
	w.Observer = func(outcome worker.Outcome) {
		counters.Add(metrics.WorkerClaims, "", 1)
		counters.Add(metrics.WorkerDispositions, string(outcome.Disposition), 1)
		counters.Add(metrics.WorkerRunSeconds, "", outcome.Duration.Seconds())
		counters.Observe(metrics.WorkerRunDuration, outcome.Duration.Seconds())
		if outcome.Takeover {
			counters.Add(metrics.WorkerTakeovers, "", 1)
		}
		if outcome.Disposition == worker.Fenced {
			counters.Add(metrics.WorkerFencingRejections, "", 1)
		}
	}
	w.Config.OnRenew = func(store.RunLease) {
		counters.Add(metrics.WorkerLeaseRenewals, "", 1)
	}
}

// traceExporter returns the worker's ExportTrace hook for a collector, or nil
// when no endpoint is configured. The hook builds the run's trace from the
// store and posts it. Both steps only read, so the ledger never learns that an
// export happened.
//
// A response that is not a 2xx is an error, which the worker logs and
// otherwise ignores. The error text goes to the worker's log, so it is scrubbed
// first: the endpoint can carry userinfo or a query token, and the header value
// and the configured provider keys must not appear in a log line.
func traceExporter(s *store.Store, endpoint string, headers map[string]string) func(context.Context, string) error {
	if endpoint == "" {
		return nil
	}
	exporter := trace.Exporter{Endpoint: endpoint, Headers: headers}
	secrets := configuredSecrets()
	for _, value := range headers {
		secrets = append(secrets, value)
	}
	scrub := exportErrorScrubber(endpoint, headers)
	return func(ctx context.Context, runID string) error {
		built, err := trace.Build(ctx, s, runID, secrets...)
		if err != nil {
			return scrub(err)
		}
		delivery, err := exporter.Export(ctx, built)
		if err != nil {
			return scrub(err)
		}
		if !delivery.Delivered {
			return scrub(fmt.Errorf("collector at %s did not accept the trace: HTTP %d", delivery.Endpoint, delivery.Status))
		}
		return nil
	}
}

func writeJSON(out io.Writer, payload any) error {
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(encoded))
	return err
}
