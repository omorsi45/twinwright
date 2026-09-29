package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Claiming under contention.
//
// A queue row can be marked runnable while another worker still holds a live
// lease on that run. The obvious way to get there is an operator re-enqueueing a
// run that is currently being worked on: Enqueue resets the state and does not
// touch the lease, which is documented behaviour and is how a failed run is
// retried.
//
// Losing a race for a run is contention, not a failure. A worker that reports an
// error there is worse than one that reports no work: ClaimAndRun surfaces the
// error to its caller, so one re-enqueue could stop a worker that had other runs
// it could have picked up.

func TestClaimRunSkipsARunAnotherWorkerStillHolds(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	held := seededRun(ctx, t, s)
	free := seededRun(ctx, t, s)

	// worker-a claims the first run and is still working on it.
	if err := s.Enqueue(ctx, held.ID, 10, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, now); err != nil {
		t.Fatalf("worker-a claim: %v", err)
	}
	// An operator re-enqueues it, which makes the row runnable again while the
	// lease is still live.
	if err := s.Enqueue(ctx, held.ID, 10, now); err != nil {
		t.Fatal(err)
	}
	// A second run is genuinely available, and is enqueued later so the held run
	// sorts first and is the candidate a claimer sees first.
	if err := s.Enqueue(ctx, free.ID, 10, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	lease, entry, err := s.ClaimRun(ctx, "worker-b", time.Minute, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("worker-b claim: %v; a run held by another worker must be skipped, not reported as an error", err)
	}
	if entry.RunID != free.ID {
		t.Fatalf("worker-b claimed %s; want the free run %s", entry.RunID, free.ID)
	}
	if lease.Owner != "worker-b" {
		t.Fatalf("lease=%+v", lease)
	}

	// worker-a must still own the run it is working on: a skipped claim changes
	// nothing about the holder.
	current, err := s.RunLease(ctx, held.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Owner != "worker-a" {
		t.Fatalf("held run owner=%q want worker-a", current.Owner)
	}
}

// With nothing else to claim, a held run means no work, which is an ordinary
// idle poll rather than an error a worker has to interpret.
func TestClaimRunReportsNoWorkWhenEveryRunIsHeld(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	run := seededRun(ctx, t, s)
	if err := s.Enqueue(ctx, run.ID, 10, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, now); err != nil {
		t.Fatalf("worker-a claim: %v", err)
	}
	if err := s.Enqueue(ctx, run.ID, 10, now); err != nil {
		t.Fatal(err)
	}

	_, _, err := s.ClaimRun(ctx, "worker-b", time.Minute, now.Add(time.Second))
	if !errors.Is(err, ErrNoWork) {
		t.Fatalf("err=%v want ErrNoWork", err)
	}
}

// The expiry path must keep working: once the holder's lease lapses, the run is
// takeable with a strictly higher fence. Skipping live leases must not turn into
// skipping dead ones, which would strand every crashed run.
func TestClaimRunStillTakesOverAnExpiredLease(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	run := seededRun(ctx, t, s)
	if err := s.Enqueue(ctx, run.ID, 10, now); err != nil {
		t.Fatal(err)
	}
	first, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, now)
	if err != nil {
		t.Fatalf("worker-a claim: %v", err)
	}

	// worker-a dies. Its lease lapses and nothing releases it.
	lease, entry, err := s.ClaimRun(ctx, "worker-b", time.Minute, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("takeover claim: %v", err)
	}
	if lease.Owner != "worker-b" || lease.Token <= first.Token {
		t.Fatalf("takeover lease=%+v first=%+v", lease, first)
	}
	if entry.Attempts != 2 {
		t.Fatalf("attempts=%d want 2", entry.Attempts)
	}
}

// A worker re-claiming its own run must still work: a reconnecting worker needs a
// fresh, strictly higher fence rather than a refusal.
func TestClaimRunLetsTheSameOwnerReclaim(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	run := seededRun(ctx, t, s)
	if err := s.Enqueue(ctx, run.ID, 10, now); err != nil {
		t.Fatal(err)
	}
	first, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Enqueue(ctx, run.ID, 10, now); err != nil {
		t.Fatal(err)
	}
	again, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, now.Add(time.Second))
	if err != nil {
		t.Fatalf("same owner re-claim: %v", err)
	}
	if again.Token <= first.Token {
		t.Fatalf("re-claim fence %d did not exceed %d", again.Token, first.Token)
	}
}
