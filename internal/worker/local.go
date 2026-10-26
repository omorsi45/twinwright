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

	lease, err := s.AcquireRunLease(ctx, runID, cfg.ID, cfg.LeaseTTL, cfg.Clock.Now())
	if err != nil {
		return store.Run{}, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := &Worker{Store: s, Config: cfg}
	renewalLost := make(chan struct{})
	renewalDone := make(chan struct{})
	go func() {
		defer close(renewalDone)
		w.renew(runCtx, lease, cancel, renewalLost)
	}()

	run, fnErr := fn(runCtx, lease.Fence())
	cancel()
	<-renewalDone

	lost := false
	select {
	case <-renewalLost:
		lost = true
	default:
	}

	// Release even when ctx is cancelled: cancellation is the Ctrl-C path, and
	// it is exactly when the lease must not be left behind. A release that is
	// rejected as fenced means the lease was already lost, which is reported
	// below from the renewal signal and must not hide fn's own outcome.
	releaseErr := s.ReleaseRunLease(context.WithoutCancel(ctx), lease, cfg.Clock.Now())

	switch {
	case lost && fnErr != nil:
		if errors.Is(fnErr, store.ErrFenced) {
			return run, fnErr
		}
		return run, fmt.Errorf("%w: lease on run %s was lost during local execution: %w", store.ErrFenced, runID, fnErr)
	case lost:
		return run, fmt.Errorf("%w: lease on run %s was lost during local execution", store.ErrFenced, runID)
	case fnErr != nil:
		return run, fnErr
	case releaseErr != nil && !errors.Is(releaseErr, store.ErrFenced):
		return run, fmt.Errorf("release lease on run %s: %w", runID, releaseErr)
	}
	return run, nil
}
