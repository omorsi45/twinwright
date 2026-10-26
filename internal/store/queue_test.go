package store

import (
	"context"
	"encoding/json"
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

// TestEnqueueOnALiveLeaseThatLaterExpiresIsATakeover covers an operator retry
// that lands while a worker still holds the run. Enqueue resets the queue row to
// runnable and leaves the lease alone, so when that lease later expires the
// claim goes through the runnable branch even though it displaces a dead owner.
// The outcome flag has to say so, and has to agree with the ownership log row
// written in the same claim.
func TestEnqueueOnALiveLeaseThatLaterExpiresIsATakeover(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	run := seededRun(ctx, t, s)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	if err := s.Enqueue(ctx, run.ID, 10, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(ctx, run.ID, 10, now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	queued, err := s.QueueEntryFor(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.State != QueueRunnable {
		t.Fatalf("queue state=%q want runnable after the re-enqueue", queued.State)
	}

	later := now.Add(2 * time.Minute)
	lease, entry, err := s.ClaimRun(ctx, "worker-b", time.Minute, later)
	if err != nil {
		t.Fatalf("claim after the lease expired: %v", err)
	}
	if !entry.Takeover {
		t.Fatalf("a claim that displaced an expired lease was not marked as a takeover: %+v", entry)
	}

	history, err := s.Ownership(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var logged map[string]any
	for _, event := range history {
		if event.Kind == OwnershipAcquired && event.Fence == lease.Token {
			if err = json.Unmarshal(event.Detail, &logged); err != nil {
				t.Fatal(err)
			}
		}
	}
	if logged == nil {
		t.Fatalf("no lease.acquired row for fence %d in %+v", lease.Token, history)
	}
	if logged["takeover"] != entry.Takeover {
		t.Fatalf("ownership log takeover=%v but the claim reported %v", logged["takeover"], entry.Takeover)
	}
}

// TestFirstClaimAndRetryAreNotTakeovers keeps the other side honest: a lease
// row that holds no owner, whether never held or released after an error, is not
// a dead worker's, so claiming it is not a takeover.
func TestFirstClaimAndRetryAreNotTakeovers(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	run := seededRun(ctx, t, s)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	if err := s.Enqueue(ctx, run.ID, 10, now); err != nil {
		t.Fatal(err)
	}
	first, entry, err := s.ClaimRun(ctx, "worker-a", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Takeover {
		t.Fatalf("a first claim was marked as a takeover: %+v", entry)
	}
	if err = s.RequeueRun(ctx, first, now, now, "transient"); err != nil {
		t.Fatal(err)
	}
	_, entry, err = s.ClaimRun(ctx, "worker-b", time.Minute, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if entry.Takeover {
		t.Fatalf("a retry after a released lease was marked as a takeover: %+v", entry)
	}
}
