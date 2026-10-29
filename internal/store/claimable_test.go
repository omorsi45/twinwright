package store

import (
	"context"
	"testing"
	"time"
)

// OldestClaimableSince and ClaimRun read the same predicate, so this walks the
// states a queue entry can be in and requires them to agree: the first reports
// an entry exactly when the second can take one, and dates it from when it became
// claimable. It runs on both backends because the date is computed in SQL.
func checkOldestClaimableSince(t *testing.T, open func(*testing.T) *Store) {
	ctx := context.Background()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, s *Store)
		// want is when the oldest claimable entry became claimable, or the zero
		// time when nothing is claimable.
		want time.Time
	}{
		{"empty queue", func(*testing.T, *Store) {}, time.Time{}},
		{"runnable and available", func(t *testing.T, s *Store) {
			enqueueRun(ctx, t, s, ago(time.Minute))
		}, ago(time.Minute)},
		{"runnable in backoff", func(t *testing.T, s *Store) {
			enqueueRun(ctx, t, s, now.Add(time.Minute))
		}, time.Time{}},
		{"leased with a live lease", func(t *testing.T, s *Store) {
			enqueueRun(ctx, t, s, ago(time.Hour))
			claimRun(ctx, t, s, "worker-a", time.Hour, now)
		}, time.Time{}},
		{"leased with an expired lease counts from the expiry", func(t *testing.T, s *Store) {
			enqueueRun(ctx, t, s, ago(time.Hour))
			claimRun(ctx, t, s, "worker-a", time.Minute, ago(30*time.Minute))
		}, ago(29 * time.Minute)},
		{"runnable with a live foreign lease", func(t *testing.T, s *Store) {
			run := enqueueRun(ctx, t, s, ago(time.Hour))
			claimRun(ctx, t, s, "worker-a", time.Hour, now)
			reenqueue(ctx, t, s, run, ago(time.Minute))
		}, time.Time{}},
		{"re-enqueued after the lease expired counts from the enqueue", func(t *testing.T, s *Store) {
			run := enqueueRun(ctx, t, s, ago(time.Hour))
			claimRun(ctx, t, s, "worker-a", time.Minute, ago(30*time.Minute))
			reenqueue(ctx, t, s, run, ago(time.Minute))
		}, ago(time.Minute)},
		{"re-enqueued before the lease expired counts from the expiry", func(t *testing.T, s *Store) {
			run := enqueueRun(ctx, t, s, ago(2*time.Hour))
			claimRun(ctx, t, s, "worker-a", time.Minute, ago(90*time.Minute))
			reenqueue(ctx, t, s, run, ago(100*time.Minute))
		}, ago(89 * time.Minute)},
		{"the longest-waiting of several", func(t *testing.T, s *Store) {
			enqueueRun(ctx, t, s, ago(5*time.Minute))
			enqueueRun(ctx, t, s, ago(10*time.Minute))
			enqueueRun(ctx, t, s, ago(7*time.Minute))
		}, ago(10 * time.Minute)},
		{"an expired lease older than a runnable entry wins", func(t *testing.T, s *Store) {
			enqueueRun(ctx, t, s, ago(5*time.Minute))
			enqueueRun(ctx, t, s, ago(time.Hour))
			// Claims the entry enqueued an hour ago, the oldest by enqueue time.
			claimRun(ctx, t, s, "worker-a", time.Minute, ago(30*time.Minute))
		}, ago(29 * time.Minute)},
		{"finished", func(t *testing.T, s *Store) {
			enqueueRun(ctx, t, s, ago(time.Hour))
			lease := claimRun(ctx, t, s, "worker-a", time.Hour, ago(time.Minute))
			if err := s.FinishRun(ctx, lease, now); err != nil {
				t.Fatal(err)
			}
		}, time.Time{}},
		{"failed", func(t *testing.T, s *Store) {
			enqueueRun(ctx, t, s, ago(time.Hour))
			lease := claimRun(ctx, t, s, "worker-a", time.Hour, ago(time.Minute))
			if err := s.FailRun(ctx, lease, now, "boom"); err != nil {
				t.Fatal(err)
			}
		}, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t)
			tc.setup(t, s)
			since, found, err := s.OldestClaimableSince(ctx, now)
			if err != nil {
				t.Fatal(err)
			}
			if wantFound := !tc.want.IsZero(); found != wantFound {
				t.Fatalf("found=%v since=%q, want found=%v", found, since, wantFound)
			}
			if found && since != CoordinationTime(tc.want) {
				t.Fatalf("since=%q want %q", since, CoordinationTime(tc.want))
			}
			// What the claim query can take is the same set.
			_, _, claimErr := s.ClaimRun(ctx, "worker-z", time.Minute, now)
			if claimable := claimErr == nil; claimable != found {
				t.Fatalf("ClaimRun err=%v, but OldestClaimableSince found=%v", claimErr, found)
			}
		})
	}
}

func enqueueRun(ctx context.Context, t *testing.T, s *Store, availableAt time.Time) string {
	t.Helper()
	run := seededRun(ctx, t, s)
	if err := s.Enqueue(ctx, run.ID, 10, availableAt); err != nil {
		t.Fatal(err)
	}
	return run.ID
}

func reenqueue(ctx context.Context, t *testing.T, s *Store, runID string, availableAt time.Time) {
	t.Helper()
	if err := s.Enqueue(ctx, runID, 10, availableAt); err != nil {
		t.Fatal(err)
	}
}

func claimRun(ctx context.Context, t *testing.T, s *Store, owner string, ttl time.Duration, at time.Time) RunLease {
	t.Helper()
	lease, _, err := s.ClaimRun(ctx, owner, ttl, at)
	if err != nil {
		t.Fatalf("%s claim: %v", owner, err)
	}
	return lease
}

func TestOldestClaimableSince(t *testing.T) {
	checkOldestClaimableSince(t, leaseTestStore)
}

func TestPostgresOldestClaimableSince(t *testing.T) {
	checkOldestClaimableSince(t, func(t *testing.T) *Store { return openTestPostgres(context.Background(), t) })
}
