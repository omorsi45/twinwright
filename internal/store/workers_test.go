package store

import (
	"context"
	"testing"
	"time"
)

// A worker row is the fleet's record of a process. RemoveWorker is how a worker
// that stops cleanly takes its own row out, so the fleet is not a list of every
// process that ever started. A row whose worker still owns a live lease is not
// removed, whatever asks: the lease is an unfinished run, and the row is how an
// operator finds who holds it.
func checkRemoveWorker(t *testing.T, open func(*testing.T) *Store) {
	ctx := context.Background()
	s := open(t)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"idle", "holder", "lapsed"} {
		if err := s.RegisterWorker(ctx, id, "host", now); err != nil {
			t.Fatal(err)
		}
	}
	// lapsed claimed a run an hour ago with a one minute lease and never came back.
	enqueueRun(ctx, t, s, now.Add(-2*time.Hour))
	claimRun(ctx, t, s, "lapsed", time.Minute, now.Add(-time.Hour))
	// holder claimed one a moment ago and is still working on it.
	enqueueRun(ctx, t, s, now.Add(-time.Minute))
	claimRun(ctx, t, s, "holder", time.Hour, now)

	ids := func() []string {
		t.Helper()
		workers, err := s.Workers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, worker := range workers {
			out = append(out, worker.ID)
		}
		return out
	}
	has := func(id string) bool {
		for _, got := range ids() {
			if got == id {
				return true
			}
		}
		return false
	}

	removed, err := s.RemoveWorker(ctx, "holder", now)
	if err != nil || removed || !has("holder") {
		t.Fatalf("holder: removed=%v err=%v still listed=%v; a row that owns a live lease must stay", removed, err, has("holder"))
	}
	for _, id := range []string{"idle", "lapsed"} {
		removed, err = s.RemoveWorker(ctx, id, now)
		if err != nil || !removed || has(id) {
			t.Fatalf("%s: removed=%v err=%v still listed=%v; a row that owns no live lease goes", id, removed, err, has(id))
		}
	}
	// Removing what is not there is not an error, and the same call twice is safe.
	if removed, err = s.RemoveWorker(ctx, "idle", now); err != nil || removed {
		t.Fatalf("second removal: removed=%v err=%v", removed, err)
	}
	if got := ids(); len(got) != 1 || got[0] != "holder" {
		t.Fatalf("workers left = %v, want [holder]", got)
	}
	// Once its lease has lapsed the holder's row can go too.
	if removed, err = s.RemoveWorker(ctx, "holder", now.Add(2*time.Hour)); err != nil || !removed {
		t.Fatalf("holder after its lease lapsed: removed=%v err=%v", removed, err)
	}
}

func TestRemoveWorker(t *testing.T) {
	checkRemoveWorker(t, leaseTestStore)
}

func TestPostgresRemoveWorker(t *testing.T) {
	checkRemoveWorker(t, func(t *testing.T) *Store { return openTestPostgres(context.Background(), t) })
}
