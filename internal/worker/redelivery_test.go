package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

// TestCommittedCallIsAnsweredFromTheCacheUnderTheNewOwner is the worker-level
// proof of the call-ID cache that TestDuplicateDeliveryDoesNotDuplicateCommittedEffects
// is not: that test redelivers a finished run, which returns at the completed
// status before any call is dispatched, so no call ID is ever presented twice.
// Here a refund is committed under worker A's fence, A dies, B takes the run over
// with a higher fence, and B presents the same call ID. It must get the saved
// result back and commit nothing: no second refund, no new ledger event.
func TestCommittedCallIsAnsweredFromTheCacheUnderTheNewOwner(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"charge_id": "CH-1002", "amount_cents": 500, "reason": "duplicate charge"}

	leaseA, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	fenceA := leaseA.Fence()
	first, err := (&dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &fenceA, Clock: clock.Clock()}).
		Invoke(ctx, run.ID, "call-refund", "createRefund", args)
	if err != nil || first.Status != 201 {
		t.Fatalf("worker-a refund: %+v %v", first, err)
	}
	counts := func() (refunds, events int) {
		current, runErr := s.Run(ctx, run.ID)
		if runErr != nil {
			t.Fatal(runErr)
		}
		if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", current.WorldID).Scan(&refunds); err != nil {
			t.Fatal(err)
		}
		ledger, eventsErr := s.Events(ctx, run.ID)
		if eventsErr != nil {
			t.Fatal(eventsErr)
		}
		return refunds, len(ledger)
	}
	refundsBefore, eventsBefore := counts()
	if refundsBefore != 1 {
		t.Fatalf("setup: refunds=%d want 1", refundsBefore)
	}

	// Worker A dies. B claims after the lease expires, with a higher fence.
	clock.Advance(2 * time.Minute)
	leaseB, entry, err := s.ClaimRun(ctx, "worker-b", time.Minute, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !entry.Takeover || leaseB.Token <= leaseA.Token {
		t.Fatalf("setup: B did not take over: %+v fences %d then %d", entry, leaseA.Token, leaseB.Token)
	}
	fenceB := leaseB.Fence()
	newOwner := &dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &fenceB, Clock: clock.Clock()}

	second, err := newOwner.Invoke(ctx, run.ID, "call-refund", "createRefund", args)
	if err != nil {
		t.Fatalf("the new owner was refused a committed call: %v", err)
	}
	if second.Status != first.Status || string(second.Body) != string(first.Body) {
		t.Fatalf("the new owner got %+v, want the saved result %+v", second, first)
	}
	if refunds, events := counts(); refunds != refundsBefore || events != eventsBefore {
		t.Fatalf("answering a committed call changed the run: refunds %d to %d, ledger events %d to %d", refundsBefore, refunds, eventsBefore, events)
	}

	// The same ID with different arguments is refused, not answered from the
	// cache, and not as a fencing failure: the new owner is entitled to the run.
	other := map[string]any{"charge_id": "CH-1002", "amount_cents": 999, "reason": "duplicate charge"}
	if _, err = newOwner.Invoke(ctx, run.ID, "call-refund", "createRefund", other); err == nil || errors.Is(err, store.ErrFenced) {
		t.Fatalf("a reused call ID with other arguments: err=%v, want a refusal that is not ErrFenced", err)
	}
	if refunds, events := counts(); refunds != refundsBefore || events != eventsBefore {
		t.Fatalf("a refused reuse changed the run: refunds %d to %d, ledger events %d to %d", refundsBefore, refunds, eventsBefore, events)
	}
}
