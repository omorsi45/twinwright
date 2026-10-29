package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestReleaseAndItsOwnershipRowCommitTogether: the release and the log row that
// records it used to be two transactions. When the second failed, ReleaseRunLease
// returned an error for a lease that was already free, and the log had no release
// row, so a caller reported "lease not released" about a released lease. A trigger
// makes the log row fail, which is what a deadline expiring between the two
// statements looks like from the database side.
func TestReleaseAndItsOwnershipRowCommitTogether(t *testing.T) {
	ctx := context.Background()
	s := leaseTestStore(t)
	run := seededRun(ctx, t, s)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	lease, err := s.AcquireRunLease(ctx, run.ID, "owner", time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = s.DB.ExecContext(ctx, `CREATE TRIGGER refuse_release_row BEFORE INSERT ON run_ownership_log
WHEN NEW.kind='lease.released' BEGIN SELECT RAISE(ABORT, 'ownership log refused the row'); END`); err != nil {
		t.Fatal(err)
	}
	err = s.ReleaseRunLease(ctx, lease, now)
	if err == nil || !strings.Contains(err.Error(), "ownership log refused the row") {
		t.Fatalf("ReleaseRunLease err=%v, want the log failure", err)
	}
	held, err := s.RunLease(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Owner != "owner" || held.Token != lease.Token {
		t.Fatalf("the release went through although its log row failed: %+v", held)
	}

	if _, err = s.DB.ExecContext(ctx, `DROP TRIGGER refuse_release_row`); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseRunLease(ctx, lease, now); err != nil {
		t.Fatalf("release after the failure: %v", err)
	}
	history, err := s.Ownership(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, event := range history {
		kinds = append(kinds, event.Kind)
	}
	if got := strings.Join(kinds, ","); got != OwnershipAcquired+","+OwnershipReleased {
		t.Fatalf("ownership log kinds=%s, want one acquisition and one release", got)
	}
}
