package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestDeliveryLimitWriteIsFenced proves the delivery-limit failure is a fenced
// write like every other: a worker that lost the run to a takeover cannot fail
// it, and the rejection leaves the run and its ledger untouched.
func TestDeliveryLimitWriteIsFenced(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	run := seededRun(ctx, t, s)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	if err := s.Enqueue(ctx, run.ID, 10, now); err != nil {
		t.Fatal(err)
	}
	stale, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * time.Minute)
	fresh, entry, err := s.ClaimRun(ctx, "worker-b", time.Minute, later)
	if err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if !entry.Takeover {
		t.Fatalf("a claim that displaced an expired lease was not marked as a takeover: %+v", entry)
	}
	if fresh.Token <= stale.Token {
		t.Fatalf("takeover fence %d did not exceed %d", fresh.Token, stale.Token)
	}
	before, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	err = s.FailRunAtDeliveryLimit(ctx, stale, later, 2, 1)
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("stale delivery-limit write err=%v want ErrFenced", err)
	}

	current, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != run.Status {
		t.Fatalf("run status changed from %q to %q", run.Status, current.Status)
	}
	after, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("events grew from %d to %d on a fenced write", len(before), len(after))
	}
	queued, err := s.QueueEntryFor(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.State != QueueLeased {
		t.Fatalf("queue state=%q want leased: a fenced write must not touch the entry", queued.State)
	}
}
