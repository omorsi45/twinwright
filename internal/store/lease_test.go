package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func leaseTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/lease.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seededRun(ctx context.Context, t *testing.T, s *Store) Run {
	t.Helper()
	world, err := s.Seed(ctx, 42, "digest")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "task", "")
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestAcquireRunLeaseIssuesMonotonicFences(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	run := seededRun(ctx, t, s)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	first, err := s.AcquireRunLease(ctx, run.ID, "worker-a", time.Minute, now)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if first.Token != 1 || first.Owner != "worker-a" {
		t.Fatalf("first lease=%+v", first)
	}

	// A live lease held by someone else must not be stealable.
	if _, err = s.AcquireRunLease(ctx, run.ID, "worker-b", time.Minute, now.Add(time.Second)); err == nil {
		t.Fatal("worker-b stole a live lease")
	}

	// After expiry the run is takeable, with a strictly higher fence.
	second, err := s.AcquireRunLease(ctx, run.ID, "worker-b", time.Minute, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("takeover after expiry: %v", err)
	}
	if second.Token != 2 {
		t.Fatalf("takeover fence=%d want 2", second.Token)
	}

	// Re-acquiring as the current owner also advances the fence, so a
	// partitioned worker that reconnects cannot reuse an old token.
	third, err := s.AcquireRunLease(ctx, run.ID, "worker-b", time.Minute, now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if third.Token != 3 {
		t.Fatalf("re-acquire fence=%d want 3", third.Token)
	}
}

func TestRenewRunLeaseRequiresCurrentOwnerAndFence(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	run := seededRun(ctx, t, s)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	lease, err := s.AcquireRunLease(ctx, run.ID, "worker-a", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := s.RenewRunLease(ctx, lease, time.Minute, now.Add(30*time.Second))
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if !renewed.ExpiresAt.After(lease.ExpiresAt) {
		t.Fatalf("renew did not extend: %s vs %s", renewed.ExpiresAt, lease.ExpiresAt)
	}
	stale := lease
	stale.Token = lease.Token - 1
	if _, err = s.RenewRunLease(ctx, stale, time.Minute, now.Add(31*time.Second)); err == nil {
		t.Fatal("renew accepted a stale fence")
	}
	foreign := renewed
	foreign.Owner = "worker-b"
	if _, err = s.RenewRunLease(ctx, foreign, time.Minute, now.Add(32*time.Second)); err == nil {
		t.Fatal("renew accepted a foreign owner")
	}
	// An expired lease cannot be renewed: the run may already be owned by a
	// newer worker, so renewal has to go back through acquire.
	if _, err = s.RenewRunLease(ctx, renewed, time.Minute, now.Add(10*time.Minute)); err == nil {
		t.Fatal("renew resurrected an expired lease")
	}
}

// GuardFence is the single choke point every worker write passes through. This
// is the test the whole fencing design exists for.
func TestGuardFenceRejectsStaleWorkerAfterTakeover(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	run := seededRun(ctx, t, s)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	stale, err := s.AcquireRunLease(ctx, run.ID, "worker-a", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	// worker-a commits once while it legitimately holds the run.
	if err = s.withGuardedTx(ctx, stale.Fence(), now.Add(time.Second), func(tx *sql.Tx) error {
		return AppendEventTx(ctx, tx, run.ID, "model.request", map[string]any{"step": 1})
	}); err != nil {
		t.Fatalf("owner write rejected: %v", err)
	}

	// worker-a stalls; the lease expires; worker-b takes over.
	fresh, err := s.AcquireRunLease(ctx, run.ID, "worker-b", time.Minute, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Fence().Token <= stale.Fence().Token {
		t.Fatalf("takeover fence %d did not advance past %d", fresh.Fence().Token, stale.Fence().Token)
	}
	// worker-b makes progress.
	if err = s.withGuardedTx(ctx, fresh.Fence(), now.Add(2*time.Minute+time.Second), func(tx *sql.Tx) error {
		return AppendEventTx(ctx, tx, run.ID, "model.request", map[string]any{"step": 2})
	}); err != nil {
		t.Fatalf("new owner write rejected: %v", err)
	}

	// worker-a wakes up and tries to commit. It must be refused, and its
	// attempted event must not be in the ledger.
	err = s.withGuardedTx(ctx, stale.Fence(), now.Add(2*time.Minute+2*time.Second), func(tx *sql.Tx) error {
		return AppendEventTx(ctx, tx, run.ID, "model.request", map[string]any{"step": 99})
	})
	if err == nil {
		t.Fatal("a stale worker committed after takeover")
	}
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("stale commit error=%v want ErrFenced", err)
	}
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if string(event.Payload) == `{"step":99}` {
			t.Fatal("the rejected write reached the ledger")
		}
		// Ownership history must never enter the run ledger: fork lineage and
		// checkpoint reconstruction digest this prefix, so an identical agent
		// trajectory has to produce an identical ledger no matter which
		// worker ran it or how often the lease changed hands.
		if strings.HasPrefix(event.Type, "lease.") || strings.HasPrefix(event.Type, "fence.") {
			t.Fatalf("ownership event %q leaked into the run ledger", event.Type)
		}
	}
	if len(events) != 3 {
		t.Fatalf("events=%d want 3 (execution.started plus two accepted writes)", len(events))
	}

	// The ownership audit log, which replay ignores, is where the history
	// lives, including the stale worker's refused attempt.
	if err = s.RecordFenceRejection(ctx, stale.Fence(), now.Add(2*time.Minute+2*time.Second), err2Reason(err)); err != nil {
		t.Fatalf("record rejection: %v", err)
	}
	history, err := s.Ownership(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var acquired, rejected int
	for _, entry := range history {
		switch entry.Kind {
		case OwnershipAcquired:
			acquired++
		case OwnershipFenced:
			rejected++
		}
	}
	if acquired != 2 {
		t.Fatalf("ownership log recorded %d acquisitions, want 2", acquired)
	}
	if rejected != 1 {
		t.Fatalf("ownership log recorded %d fencing rejections, want 1", rejected)
	}
}

func err2Reason(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Expiry alone must stop a commit, even with no competing worker: after the
// lease lapses another worker may take over at any instant.
func TestGuardFenceRejectsExpiredLeaseWithoutTakeover(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	run := seededRun(ctx, t, s)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	lease, err := s.AcquireRunLease(ctx, run.ID, "worker-a", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	err = s.withGuardedTx(ctx, lease.Fence(), now.Add(5*time.Minute), func(tx *sql.Tx) error {
		return AppendEventTx(ctx, tx, run.ID, "model.request", map[string]any{"step": 1})
	})
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("expired-lease commit err=%v want ErrFenced", err)
	}
}

// The monotonic committed-fence record is what keeps fencing correct when
// clocks disagree: a lower fence is refused regardless of the wall clock.
func TestGuardFenceRejectsLowerFenceEvenWhenTheLeaseLooksLive(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	run := seededRun(ctx, t, s)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	old, err := s.AcquireRunLease(ctx, run.ID, "worker-a", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := s.AcquireRunLease(ctx, run.ID, "worker-a", time.Hour, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.withGuardedTx(ctx, newer.Fence(), now.Add(2*time.Minute), func(tx *sql.Tx) error {
		return AppendEventTx(ctx, tx, run.ID, "model.request", map[string]any{"step": 1})
	}); err != nil {
		t.Fatal(err)
	}
	// The old token is still inside its TTL, yet a newer fence has already
	// committed, so it must lose.
	err = s.withGuardedTx(ctx, old.Fence(), now.Add(3*time.Minute), func(tx *sql.Tx) error {
		return AppendEventTx(ctx, tx, run.ID, "model.request", map[string]any{"step": 2})
	})
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("superseded fence err=%v want ErrFenced", err)
	}
}

func TestReleaseRunLeaseMakesTheRunClaimableWithoutResettingFences(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	run := seededRun(ctx, t, s)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	lease, err := s.AcquireRunLease(ctx, run.ID, "worker-a", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseRunLease(ctx, lease, now.Add(time.Second)); err != nil {
		t.Fatalf("release: %v", err)
	}
	next, err := s.AcquireRunLease(ctx, run.ID, "worker-b", time.Minute, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if next.Fence().Token <= lease.Fence().Token {
		t.Fatalf("fence went backwards after release: %d then %d", lease.Fence().Token, next.Fence().Token)
	}
	// The released holder must not be able to write again.
	err = s.withGuardedTx(ctx, lease.Fence(), now.Add(3*time.Second), func(tx *sql.Tx) error {
		return AppendEventTx(ctx, tx, run.ID, "model.request", map[string]any{"step": 1})
	})
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("released holder err=%v want ErrFenced", err)
	}
}

func TestRunLeaseReportsCurrentOwnership(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	run := seededRun(ctx, t, s)
	if _, err := s.RunLease(ctx, run.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unowned run lease err=%v want sql.ErrNoRows", err)
	}
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	if _, err := s.AcquireRunLease(ctx, run.ID, "worker-a", time.Minute, now); err != nil {
		t.Fatal(err)
	}
	lease, err := s.RunLease(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Owner != "worker-a" || lease.Fence().Token != 1 {
		t.Fatalf("lease=%+v", lease)
	}
}
