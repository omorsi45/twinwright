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
	"twinwright/internal/dispatch"
	"twinwright/internal/metrics"
	"twinwright/internal/store"
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

// queueCommand reports queue depth, the registered fleet and per-run ownership.
func queueCommand(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("queue", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dbPath := fs.String("db", "twinwright.db", "SQLite path or postgres:// DSN")
	runID := fs.String("run", "", "show one run's queue entry and ownership history")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	s, err := store.OpenDSN(ctx, *dbPath)
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
	baseURL := fs.String("base-url", os.Getenv("OPENAI_BASE_URL"), "base URL for openai-compatible providers")
	if err := fs.Parse(args[1:]); err != nil {
		return err
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
		provider, err := selectProvider(run.Provider, run.Model, run.Scenario, *baseURL)
		if err != nil {
			return store.Run{}, err
		}
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
	})

	// Per-process event counters. Queue depth, run statuses and the
	// ledger-derived gauges are read from the database at scrape time instead,
	// so they cannot drift from the ledger.
	counters := metrics.NewCounters()
	w.Observer = func(outcome worker.Outcome) {
		counters.Add(metrics.WorkerClaims, "", 1)
		counters.Add(metrics.WorkerDispositions, string(outcome.Disposition), 1)
		counters.Add(metrics.WorkerRunSeconds, "", outcome.Duration.Seconds())
		if outcome.Takeover {
			counters.Add(metrics.WorkerTakeovers, "", 1)
		}
		if outcome.Disposition == worker.Fenced {
			counters.Add(metrics.WorkerFencingRejections, "", 1)
		}
	}

	// Ctrl-C and SIGTERM cancel the context. An in-flight run is abandoned
	// rather than force-committed; its lease lapses and another worker picks it
	// up, which is the same path as a crash and is already tested.
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
	for *maxRuns == 0 || len(outcomes) < *maxRuns {
		outcome, err := w.ClaimAndRun(signalCtx)
		if errors.Is(err, store.ErrNoWork) {
			if *drain {
				break
			}
			select {
			case <-signalCtx.Done():
				break
			case <-time.After(*poll):
				continue
			}
			break
		}
		if err != nil {
			return err
		}
		outcomes = append(outcomes, outcome)
	}
	return writeJSON(out, map[string]any{
		"worker":   w.ID(),
		"claims":   len(outcomes),
		"outcomes": outcomes,
		"delivery": "at_least_once",
		"backend":  string(s.Dialect),
	})
}

func writeJSON(out io.Writer, payload any) error {
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, string(encoded))
	return err
}
