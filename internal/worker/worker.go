// Package worker is Twinwright's multi-worker execution runtime.
//
// Delivery is at-least-once. A worker claims a queued run under a fenced lease,
// executes it, and marks it finished. If the worker dies, its lease expires and
// another worker claims the same run with a strictly higher fence, so the same
// unit of work is attempted again. Two properties make the repeat safe, and
// neither of them is exactly-once delivery, which this package does not claim:
//
//   - Tool effects are idempotent by call ID. A repeated tool call returns the
//     result recorded the first time instead of committing a second effect.
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
	// MaxAttempts bounds retries before a run is marked failed. At-least-once
	// delivery without a bound is an infinite retry loop.
	MaxAttempts int
	// Clock is for deterministic tests; nil means time.Now.
	Clock store.Clock
	// Logger defaults to slog.Default.
	Logger *slog.Logger
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
	outcome := Outcome{WorkerID: w.Config.ID, RunID: lease.RunID, Fence: lease.Token, Attempt: entry.Attempts, Takeover: entry.Attempts > 1}
	started := now

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
	}
	return w.observe(outcome), nil
}

func (w *Worker) observe(outcome Outcome) Outcome {
	if w.Observer != nil {
		w.Observer(outcome)
	}
	w.Config.Logger.Info("claim finished",
		"worker", outcome.WorkerID, "run", outcome.RunID, "fence", outcome.Fence,
		"attempt", outcome.Attempt, "disposition", string(outcome.Disposition),
		"run_status", outcome.RunStatus, "error", outcome.Err)
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
