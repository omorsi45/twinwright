package worker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"twinwright/internal/store"
)

// queuedRunForTiming creates a run and enqueues it far enough in the past that a
// clock handed to the worker can run backwards from its base and still be later
// than the enqueue time.
func queuedRunForTiming(ctx context.Context, t *testing.T, s *store.Store, base time.Time) store.Run {
	t.Helper()
	world, err := s.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "test", "test-model", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Enqueue(ctx, run.ID, 10, base.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	return run
}

// A claim is timed with the process's monotonic clock, not with the clock the
// worker coordinates leases by. That clock is read as wall time and converted to
// UTC, which drops the monotonic reading, so an NTP step or a VM migration during
// a claim made the duration negative and the run-seconds counter and the
// histogram sum went down. This clock runs backwards one second per reading, the
// worst case of that, and the claim must still report the time it really took.
func TestClaimDurationIgnoresABackwardClockStep(t *testing.T) {
	ctx := context.Background()
	s := openStore(ctx, t)
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	run := queuedRunForTiming(ctx, t, s, base)

	var readings atomic.Int64
	backwards := func() time.Time { return base.Add(-time.Duration(readings.Add(1)) * time.Second) }
	const held = 40 * time.Millisecond
	execute := func(context.Context, store.RunLease, store.QueueEntry) (store.Run, error) {
		time.Sleep(held)
		return store.Run{ID: run.ID, Status: "completed"}, nil
	}
	w := New(s, execute, Config{ID: "worker-a", LeaseTTL: time.Minute, Clock: backwards, Logger: quietLogger()})
	var observed []Outcome
	w.Observer = func(o Outcome) { observed = append(observed, o) }
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != Finished {
		t.Fatalf("outcome=%+v", outcome)
	}
	if outcome.Duration < held {
		t.Fatalf("duration=%v after a run that held the claim for %v on a clock that ran backwards", outcome.Duration, held)
	}
	if outcome.Duration > 30*time.Second {
		t.Fatalf("duration=%v, far longer than the run took", outcome.Duration)
	}
	if len(observed) != 1 || observed[0].Duration != outcome.Duration {
		t.Fatalf("the observer saw %+v, want the same duration as the returned outcome", observed)
	}
}
