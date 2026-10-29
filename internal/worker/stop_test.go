package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"twinwright/internal/store"
)

// TestWorkerStoppedMidRunReleasesAndReportsStopped: a worker that is told to stop
// (the signal context ends) while a run is executing used to try to requeue it with
// the cancelled context. The requeue failed, the caller got "requeue run: context
// canceled" as the command's error, the lease stayed held until its TTL, and the
// claim stayed counted as a delivery, so a few rolling restarts during one long run
// failed it at the delivery limit. A stop is not a failed delivery. The claim is
// released under a context the stop does not reach, the delivery it counted is given
// back, and the call returns normally.
func TestWorkerStoppedMidRunReleasesAndReportsStopped(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}

	stopCtx, stop := context.WithCancel(ctx)
	defer stop()
	started := make(chan struct{})
	blocked := func(execCtx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		close(started)
		<-execCtx.Done()
		return store.Run{}, execCtx.Err()
	}
	// One attempt in all: if the stopped delivery were kept, the next claim would
	// be past the bound and fail the run without executing it.
	w := New(s, blocked, Config{ID: "worker-a", LeaseTTL: time.Minute, MaxAttempts: 1, Backoff: time.Hour, Clock: clock.Clock(), Logger: quietLogger()})
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	type result struct {
		outcome Outcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := w.ClaimAndRun(stopCtx)
		done <- result{outcome, err}
	}()
	<-started
	stop()
	got := <-done

	if got.err != nil {
		t.Fatalf("a stop surfaced as an error: %v", got.err)
	}
	if got.outcome.Disposition != Requeued || !strings.Contains(got.outcome.Err, "stopped") {
		t.Fatalf("outcome=%+v, want the run returned to the queue with a stopped reason", got.outcome)
	}
	entry, err := s.QueueEntryFor(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.State != store.QueueRunnable || entry.Attempts != 0 {
		t.Fatalf("queue entry=%+v, want runnable with the stopped delivery given back", entry)
	}
	if entry.AvailableAt > store.CoordinationTime(clock.Now()) {
		t.Fatalf("the run is delayed until %s although the stop was not a failure (backoff is for failures)", entry.AvailableAt)
	}
	lease, err := s.RunLease(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Owner != "" {
		t.Fatalf("the stopped worker still holds the lease: %+v", lease)
	}
	kinds := ownershipKinds(ctx, t, s, run.ID)
	if got := strings.Join(kinds, ","); got != store.OwnershipAcquired+","+store.OwnershipReleased {
		t.Fatalf("ownership log kinds=%s, want an acquisition and a release and no fence rejection", got)
	}

	// The next worker takes the run at once, as its first delivery, and finishes it.
	b := New(s, realExecutor(s, manifest, clock.Clock()), Config{ID: "worker-b", LeaseTTL: time.Minute, MaxAttempts: 1, Clock: clock.Clock(), Logger: quietLogger()})
	if err = b.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := b.ClaimAndRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != Finished || outcome.Attempt != 1 || outcome.Takeover {
		t.Fatalf("the next claim=%+v, want a first delivery that finishes", outcome)
	}
}

// TestReleaseClaimGivesBackOnlyTheDeliveryOfTheCurrentOwner pins the store side: the
// release is fenced, so a worker that no longer owns the run cannot undo anyone's
// delivery count.
func TestReleaseClaimGivesBackOnlyTheDeliveryOfTheCurrentOwner(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	first, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute)
	second, entry, err := s.ClaimRun(ctx, "worker-b", time.Minute, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if entry.Attempts != 2 {
		t.Fatalf("setup: attempts=%d want 2", entry.Attempts)
	}

	if err = s.ReleaseClaim(ctx, first, clock.Now(), "stale"); err == nil {
		t.Fatal("a worker that lost the run released it")
	}
	if current, queueErr := s.QueueEntryFor(ctx, run.ID); queueErr != nil || current.Attempts != 2 || current.State != store.QueueLeased {
		t.Fatalf("a refused release changed the queue entry: %+v %v", current, queueErr)
	}

	if err = s.ReleaseClaim(ctx, second, clock.Now(), "stopped"); err != nil {
		t.Fatal(err)
	}
	current, err := s.QueueEntryFor(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Attempts != 1 || current.State != store.QueueRunnable {
		t.Fatalf("queue entry=%+v, want runnable with exactly the released delivery given back", current)
	}
}
