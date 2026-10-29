package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"twinwright/internal/store"
)

// DefaultLocalLeaseTTL is shorter than the fleet default. A local process that
// is killed leaves no queue entry to recover it, so the next local command or
// worker has to wait out the lease; 15s keeps that wait short while renewal
// (a third of the TTL) still has several chances to land.
const DefaultLocalLeaseTTL = 15 * time.Second

// releaseTimeout bounds the release at the end of a local run. The release
// deliberately ignores the command's cancellation, so without a deadline of its
// own a stalled database would hold the process open after Ctrl-C. The lease
// then lapses on its own TTL, which is what a failed release already falls
// back to.
const releaseTimeout = 5 * time.Second

// releaseContext returns the context for the final release: cancellation of ctx
// does not reach it, and it ends after releaseTimeout.
func releaseContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
}

// ErrLeaseNotReleased marks a local run whose function returned without error
// but whose lease could not be released. The work is durable; only the lock is
// left to lapse, so callers must not report the run itself as failed.
var ErrLeaseNotReleased = errors.New("lease not released")

// LocalOwner names the lease owner for a command run directly from the CLI.
// The host and PID make two local processes distinct, so one cannot renew the
// other's lease.
func LocalOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("local-%s-%d", host, os.Getpid())
}

// ExecuteLocal runs fn under the same fenced lease a fleet worker would hold.
//
// Without it, `run` and `resume` executed with no fence and could write to a
// run at the same time as a worker that had claimed it. The lease is acquired
// before fn, renewed with the worker's own renewal loop while fn runs, and
// released when fn returns, even if ctx was cancelled, so an interrupted
// command does not leave the run locked until the TTL lapses.
//
// A live lease held by another owner returns an error wrapping
// store.ErrLeaseHeld, naming the holder and expiry, and fn is not called. If
// the lease is lost while fn runs, fn's context is cancelled and the result
// wraps store.ErrFenced.
func ExecuteLocal(ctx context.Context, s *store.Store, runID string, cfg Config, fn func(ctx context.Context, fence store.Fence) (store.Run, error)) (store.Run, error) {
	if cfg.ID == "" {
		cfg.ID = LocalOwner()
	}
	// The local default must be applied before withDefaults, which would
	// otherwise substitute the fleet's longer TTL.
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = DefaultLocalLeaseTTL
	}
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return store.Run{}, err
	}

	lease, err := s.AcquireRunLease(ctx, runID, cfg.ID, cfg.LeaseTTL, cfg.Clock.Now())
	if err != nil {
		return store.Run{}, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := &Worker{Store: s, Config: cfg}
	loss := &leaseLoss{}
	renewalDone := make(chan struct{})
	go func() {
		defer close(renewalDone)
		w.renew(runCtx, lease, cancel, loss)
	}()

	run, fnErr := fn(runCtx, lease.Fence())
	cancel()
	<-renewalDone

	lost := loss.lost
	// Release even when ctx is cancelled: cancellation is the Ctrl-C path, and
	// it is exactly when the lease must not be left behind. A release that is
	// rejected as fenced means the lease was already lost, which is reported
	// below from the renewal signal and must not hide fn's own outcome. Any
	// other release error (a dropped connection, a database fault) leaves the
	// run locked until the TTL lapses, so it is reported, never discarded.
	releaseCtx, stopRelease := releaseContext(ctx)
	defer stopRelease()
	releaseErr := s.ReleaseRunLease(releaseCtx, lease, cfg.Clock.Now())
	if errors.Is(releaseErr, store.ErrFenced) {
		releaseErr = nil
	}

	var outcome error
	switch {
	case lost && loss.cause != nil && !errors.Is(loss.cause, store.ErrFenced):
		// The lease lapsed while renewals were failing; say why, not just that it
		// was lost.
		outcome = fmt.Errorf("%w: %w", store.ErrFenced, loss.cause)
	case lost && fnErr != nil && errors.Is(fnErr, store.ErrFenced):
		outcome = fnErr
	case lost && fnErr != nil:
		outcome = fmt.Errorf("%w: lease on run %s was lost during local execution: %w", store.ErrFenced, runID, fnErr)
	case lost:
		outcome = fmt.Errorf("%w: lease on run %s was lost during local execution", store.ErrFenced, runID)
	default:
		outcome = fnErr
	}
	switch {
	case releaseErr == nil:
		return run, outcome
	case outcome != nil:
		return run, errors.Join(outcome, fmt.Errorf("release lease on run %s: %w", runID, releaseErr))
	}
	// fn returned no error, so its work is durable. Say the lease was left
	// behind, not that the run failed, and name the status fn reported: the run
	// may have completed or only paused at its step budget.
	return run, fmt.Errorf("%w on run %s, which ended with status %q: %w", ErrLeaseNotReleased, runID, run.Status, releaseErr)
}
