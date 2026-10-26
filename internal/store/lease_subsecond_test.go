package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Lease expiry and queue availability are compared inside SQL, so the stored
// text has to order the way the times do. These tests pin the sub-second cases
// where a trimmed fractional part ("12:00:01Z" against "12:00:01.5Z") used to
// sort the wrong way round.

func TestRenewRejectsALeaseThatExpiredEarlierInTheSameSecond(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	run := seededRun(ctx, t, s)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	lease, err := s.AcquireRunLease(ctx, run.ID, "worker-a", time.Second, base) // expires 12:00:01.000
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	_, err = s.RenewRunLease(ctx, lease, time.Second, base.Add(1500*time.Millisecond))
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("renewed an expired lease: err=%v", err)
	}
}

func TestClaimTakesOverALeaseThatExpiredEarlierInTheSameSecond(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	run := seededRun(ctx, t, s)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	if err := s.Enqueue(ctx, run.ID, 10, base); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	first, _, err := s.ClaimRun(ctx, "worker-a", time.Second, base) // expires 12:00:01.000
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	second, _, err := s.ClaimRun(ctx, "worker-b", time.Second, base.Add(1500*time.Millisecond))
	if err != nil {
		t.Fatalf("takeover claim: %v", err)
	}
	if second.Owner != "worker-b" || second.Token != first.Token+1 {
		t.Fatalf("takeover lease=%+v after first=%+v", second, first)
	}
}

func TestRequeuedRunIsClaimableWithinTheSecondItBecomesAvailable(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	run := seededRun(ctx, t, s)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	if err := s.Enqueue(ctx, run.ID, 10, base); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	lease, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, base)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err = s.RequeueRun(ctx, lease, base, base.Add(time.Second), "retry"); err != nil { // available 12:00:01.000
		t.Fatalf("requeue: %v", err)
	}
	_, entry, err := s.ClaimRun(ctx, "worker-b", time.Minute, base.Add(1500*time.Millisecond))
	if err != nil {
		t.Fatalf("claim after availability: %v", err)
	}
	if entry.RunID != run.ID {
		t.Fatalf("claimed %q want %q", entry.RunID, run.ID)
	}
}
