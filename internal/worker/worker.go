// Package worker is Twinwright's multi-worker execution runtime.
//
// Delivery is at-least-once. A worker claims a queued run under a fenced lease,
// executes it, and marks it finished. If the worker dies, its lease expires and
// another worker claims the same run with a strictly higher fence, so the same
// unit of work is attempted again. Two properties make the repeat safe, and
// neither of them is exactly-once delivery, which this package does not claim:
//
//   - A tool call's effect, its ledger events, its saved result and its
//     transcript entry commit in one fenced transaction. Resume dispatches only
//     calls that have no tool message in the transcript, so a committed call is
//     not presented again. The call-ID cache is a second guard: it answers a
//     repeated dispatch of the same call with the recorded result instead of a
//     second effect. Model calls are not covered; a crash before a response is
//     recorded repeats the provider call. ADR 0029 has the full account.
//   - Durable writes are fenced. A worker whose lease lapsed cannot commit
//     after a newer worker owns the run, so a process that stalls and wakes up
//     later cannot corrupt the run it lost.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"twinwright/internal/store"
	"twinwright/internal/trace"
)

// Defaults chosen so a renewal has two chances to land before a lease lapses.
const (
	DefaultLeaseTTL     = 30 * time.Second
	DefaultPollInterval = time.Second
	DefaultBackoff      = 5 * time.Second
	DefaultMaxAttempts  = 3
)

// ExecuteFunc runs one claimed run to a stopping point: completion, a step
// budget pause, or an error. It receives a context that is cancelled if the
// worker loses the lease mid-flight, and must pass lease.Fence() into the
// runtime so its writes are gated.
type ExecuteFunc func(ctx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error)

// Config tunes one worker.
type Config struct {
	// ID identifies this worker across the fleet. Two processes sharing an ID
	// could renew each other's leases, so it defaults to host plus PID.
	ID string
	// Host is recorded for operators.
	Host string
	// LeaseTTL is how long a claim is valid without renewal. Shorter means
	// faster recovery from a crash and less tolerance for a slow model call.
	LeaseTTL time.Duration
	// RenewEvery defaults to LeaseTTL/3.
	RenewEvery time.Duration
	// PollInterval is the idle wait between empty claims.
	PollInterval time.Duration
	// Backoff delays a requeued run so a failing run cannot spin.
	Backoff time.Duration
	// MaxAttempts bounds deliveries before a run is marked failed, including
	// deliveries whose worker crashed and reported nothing. A claim beyond it
	// settles the run without executing it. At-least-once delivery without a
	// bound is an infinite retry loop.
	MaxAttempts int
	// Clock is for deterministic tests; nil means time.Now.
	Clock store.Clock
	// Logger defaults to slog.Default.
	Logger *slog.Logger
	// ExportTrace, when set, sends the trace of a run this worker has just
	// finished as completed. Nil means no export. It runs after the queue entry
	// is finished and before the claim is observed and logged, under a context
	// bounded by trace.DefaultExportTimeout.
	//
	// The export is best effort. Its error is logged at warn level and never
	// changes the claim's disposition: the run is complete and recorded, and a
	// collector outage must not requeue or fail it. The hook reads the run and
	// writes nothing to the ledger. Its error text reaches the log, so the hook
	// is responsible for keeping credentials out of it.
	ExportTrace func(ctx context.Context, runID string) error
	// OnRenew, when set, is called with the renewed lease after each successful
	// lease renewal. A renewal the store refuses does not call it. It runs on
	// the renewal goroutine while the run executes, so it must be quick and safe
	// for concurrent use; the metrics counters are. It observes liveness only and
	// must not write to the ledger: lease renewals are ownership bookkeeping.
	OnRenew func(store.RunLease)
}

func (c Config) withDefaults() Config {
	if c.ID == "" {
		host, _ := os.Hostname()
		if host == "" {
			host = "unknown"
		}
		c.ID = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	if c.Host == "" {
		host, _ := os.Hostname()
		c.Host = host
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = DefaultLeaseTTL
	}
	if c.RenewEvery <= 0 {
		c.RenewEvery = c.LeaseTTL / 3
		if c.RenewEvery < time.Second {
			c.RenewEvery = time.Second
		}
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.Backoff <= 0 {
		c.Backoff = DefaultBackoff
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = DefaultMaxAttempts
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Worker claims and executes queued runs.
type Worker struct {
	Store   *store.Store
	Execute ExecuteFunc
	Config  Config

	// Observer, when set, receives every outcome. The metrics endpoint and
	// tests both use this instead of scraping logs.
	Observer func(Outcome)
}

// Disposition says how one claimed run ended.
type Disposition string

const (
	// Finished: the run reached completion or its step budget.
	Finished Disposition = "finished"
	// Requeued: the attempt failed and the run will be tried again.
	Requeued Disposition = "requeued"
	// Failed: the attempt failed and retries are exhausted.
	Failed Disposition = "failed"
	// Fenced: this worker lost the run to another worker mid-flight. Nothing
	// was committed and nothing is recorded against the queue entry, because
	// the run is no longer ours to describe.
	Fenced Disposition = "fenced"
)

// Outcome is one completed claim.
type Outcome struct {
	WorkerID    string        `json:"worker_id"`
	RunID       string        `json:"run_id"`
	Fence       int64         `json:"fence"`
	Attempt     int           `json:"attempt"`
	Disposition Disposition   `json:"disposition"`
	RunStatus   string        `json:"run_status,omitempty"`
	Takeover    bool          `json:"takeover"`
	Duration    time.Duration `json:"duration_ns"`
	Err         string        `json:"error,omitempty"`
	// TraceID names the run's trace in a collector. It is derived from the run
	// ID, so it is set for every outcome that has a run, whether or not a trace
	// was exported.
	TraceID string `json:"trace_id"`
	// TraceExported is true when the worker delivered the run's trace to the
	// collector. It stays false when export is not configured, when the claim
	// did not end finished with a completed run, and when the export failed.
	TraceExported bool `json:"trace_exported,omitempty"`
}

// New returns a worker with defaults applied.
func New(s *store.Store, execute ExecuteFunc, cfg Config) *Worker {
	return &Worker{Store: s, Execute: execute, Config: cfg.withDefaults()}
}

// Register records this worker so operators can list the fleet.
func (w *Worker) Register(ctx context.Context) error {
	return w.Store.RegisterWorker(ctx, w.Config.ID, w.Config.Host, w.Config.Clock.Now())
}

// ID reports this worker's identity.
func (w *Worker) ID() string { return w.Config.ID }

// ClaimAndRun performs exactly one unit of work. It returns ErrNoWork, wrapped
// from the store, when the queue holds nothing claimable, which callers treat as
// an idle tick rather than a failure.
func (w *Worker) ClaimAndRun(ctx context.Context) (Outcome, error) {
	now := w.Config.Clock.Now()
	lease, entry, err := w.Store.ClaimRun(ctx, w.Config.ID, w.Config.LeaseTTL, now)
	if err != nil {
		return Outcome{WorkerID: w.Config.ID}, err
	}
	outcome := Outcome{WorkerID: w.Config.ID, RunID: lease.RunID, Fence: lease.Token, Attempt: entry.Attempts, Takeover: entry.Takeover,
		TraceID: trace.TraceIDFor(lease.RunID)}
	started := now

	// A crash reports no error, so the retry bound in the error path below never
	// sees a run that kills its worker. Every claim counts as a delivery, and a
	// run past its bound is settled here without executing it again.
	if entry.Attempts > w.Config.MaxAttempts {
		return w.settleOverDelivered(ctx, lease, entry, outcome)
	}

	// Renewal runs alongside execution. If the lease cannot be renewed the run
	// has moved on, so the execution context is cancelled: continuing would
	// burn model calls on work this worker is no longer allowed to commit.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	renewalLost := make(chan struct{})
	renewalDone := make(chan struct{})
	go func() {
		defer close(renewalDone)
		w.renew(runCtx, lease, cancel, renewalLost)
	}()

	run, execErr := w.Execute(runCtx, lease, entry)
	cancel()
	<-renewalDone

	lostLease := false
	select {
	case <-renewalLost:
		lostLease = true
	default:
	}

	outcome.Duration = w.Config.Clock.Now().Sub(started)
	outcome.RunStatus = run.Status

	switch {
	case errors.Is(execErr, store.ErrFenced) || lostLease:
		outcome.Disposition = Fenced
		if execErr != nil {
			outcome.Err = execErr.Error()
		} else {
			outcome.Err = "lease lost during execution"
		}
		// Best-effort audit note. The run is not ours, so a failure to record
		// the rejection must not turn into a reported error.
		if recErr := w.Store.RecordFenceRejection(ctx, lease.Fence(), w.Config.Clock.Now(), outcome.Err); recErr != nil {
			w.Config.Logger.Warn("could not record fencing rejection", "run", lease.RunID, "error", recErr)
		}
	case execErr != nil:
		reason := execErr.Error()
		if entry.Attempts >= w.Config.MaxAttempts {
			outcome.Disposition = Failed
			if err = w.Store.FailRun(ctx, lease, w.Config.Clock.Now(), reason); err != nil {
				return w.observe(outcome), fmt.Errorf("mark run failed: %w (original error: %v)", err, execErr)
			}
		} else {
			outcome.Disposition = Requeued
			at := w.Config.Clock.Now().Add(w.Config.Backoff)
			if err = w.Store.RequeueRun(ctx, lease, w.Config.Clock.Now(), at, reason); err != nil {
				return w.observe(outcome), fmt.Errorf("requeue run: %w (original error: %v)", err, execErr)
			}
		}
		outcome.Err = reason
	default:
		outcome.Disposition = Finished
		if err = w.Store.FinishRun(ctx, lease, w.Config.Clock.Now()); err != nil {
			return w.observe(outcome), fmt.Errorf("mark run finished: %w", err)
		}
		outcome = w.exportTrace(ctx, outcome)
	}
	return w.observe(outcome), nil
}

// settleOverDelivered ends a claim for a run that has been delivered more times
// than MaxAttempts allows. A run that already completed lost only its final
// bookkeeping to a crash, so it is finished; anything else is failed.
func (w *Worker) settleOverDelivered(ctx context.Context, lease store.RunLease, entry store.QueueEntry, outcome Outcome) (Outcome, error) {
	run, err := w.Store.Run(ctx, lease.RunID)
	if err != nil {
		return w.observe(outcome), fmt.Errorf("read run past delivery limit: %w", err)
	}
	outcome.RunStatus = run.Status
	if run.Status == "completed" {
		outcome.Disposition = Finished
		if err = w.Store.FinishRun(ctx, lease, w.Config.Clock.Now()); err != nil {
			return w.observe(outcome), fmt.Errorf("mark run finished: %w", err)
		}
		return w.observe(w.exportTrace(ctx, outcome)), nil
	}
	outcome.Disposition = Failed
	outcome.Err = store.DeliveryLimitReason(entry.Attempts, w.Config.MaxAttempts)
	if err = w.Store.FailRunAtDeliveryLimit(ctx, lease, w.Config.Clock.Now(), entry.Attempts, w.Config.MaxAttempts); err != nil {
		return w.observe(outcome), fmt.Errorf("fail run at delivery limit: %w", err)
	}
	outcome.RunStatus = "failed"
	return w.observe(outcome), nil
}

// exportTrace sends the trace of a run this claim finished as completed, when
// the worker was given an exporter. A run paused at its step budget, a failed
// attempt and a fenced claim all skip it: none of them leaves a finished run
// whose trace is complete. Called only after the queue entry is finished, so a
// failed FinishRun never exports for a claim that did not end.
//
// A failure is logged and otherwise ignored; see Config.ExportTrace.
func (w *Worker) exportTrace(ctx context.Context, outcome Outcome) Outcome {
	if w.Config.ExportTrace == nil || outcome.Disposition != Finished || outcome.RunStatus != "completed" {
		return outcome
	}
	exportCtx, cancel := context.WithTimeout(ctx, trace.DefaultExportTimeout)
	defer cancel()
	if err := w.Config.ExportTrace(exportCtx, outcome.RunID); err != nil {
		w.Config.Logger.Warn("trace export failed",
			"worker", outcome.WorkerID, "run", outcome.RunID, "trace_id", outcome.TraceID, "error", err)
		return outcome
	}
	outcome.TraceExported = true
	return outcome
}

func (w *Worker) observe(outcome Outcome) Outcome {
	if w.Observer != nil {
		w.Observer(outcome)
	}
	w.Config.Logger.Info("claim finished",
		"worker", outcome.WorkerID, "run", outcome.RunID, "fence", outcome.Fence,
		"attempt", outcome.Attempt, "disposition", string(outcome.Disposition),
		"run_status", outcome.RunStatus, "trace_id", outcome.TraceID, "error", outcome.Err)
	return outcome
}

// renew extends the lease until the run finishes or the lease is lost. Losing
// it cancels execution through cancel and signals lost.
func (w *Worker) renew(ctx context.Context, lease store.RunLease, cancel context.CancelFunc, lost chan struct{}) {
	ticker := time.NewTicker(w.Config.RenewEvery)
	defer ticker.Stop()
	current := lease
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renewed, err := w.Store.RenewRunLease(ctx, current, w.Config.LeaseTTL, w.Config.Clock.Now())
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				w.Config.Logger.Warn("lease renewal rejected; abandoning run",
					"worker", w.Config.ID, "run", current.RunID, "fence", current.Token, "error", err)
				close(lost)
				cancel()
				return
			}
			current = renewed
			if w.Config.OnRenew != nil {
				w.Config.OnRenew(renewed)
			}
			if err = w.Store.TouchWorker(ctx, w.Config.ID, w.Config.Clock.Now()); err != nil && ctx.Err() == nil {
				w.Config.Logger.Warn("worker heartbeat failed", "worker", w.Config.ID, "error", err)
			}
		}
	}
}

// Serve claims and runs work until ctx is cancelled, waiting PollInterval
// whenever the queue is empty.
func (w *Worker) Serve(ctx context.Context) error {
	if err := w.Register(ctx); err != nil {
		return fmt.Errorf("register worker: %w", err)
	}
	w.Config.Logger.Info("worker started", "worker", w.Config.ID, "lease_ttl", w.Config.LeaseTTL.String())
	for {
		if ctx.Err() != nil {
			w.Config.Logger.Info("worker stopped", "worker", w.Config.ID)
			return nil
		}
		_, err := w.ClaimAndRun(ctx)
		switch {
		case errors.Is(err, store.ErrNoWork):
			select {
			case <-ctx.Done():
				w.Config.Logger.Info("worker stopped", "worker", w.Config.ID)
				return nil
			case <-time.After(w.Config.PollInterval):
			}
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			w.Config.Logger.Info("worker stopped", "worker", w.Config.ID)
			return nil
		case err != nil:
			// A store-level failure is not a reason to exit the fleet: log,
			// back off, and keep serving.
			w.Config.Logger.Error("claim failed", "worker", w.Config.ID, "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(w.Config.PollInterval):
			}
		}
	}
}
