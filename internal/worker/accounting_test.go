package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"twinwright/internal/store"
)

// The counters the worker feeds are added up against each other on dashboards:
// claims against the sum of dispositions, run seconds against the histogram. These
// tests hold the places where a claim used to be counted wrongly.

func overDeliveredWorker(ctx context.Context, t *testing.T, s *store.Store, runStatus string) (*Worker, store.Run, *[]Outcome) {
	t.Helper()
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	run := queuedRunForTiming(ctx, t, s, base)
	// Five deliveries recorded against a bound of one: the next claim is past it.
	if _, err := s.DB.ExecContext(ctx, "UPDATE work_queue SET attempts=5 WHERE run_id=?", run.ID); err != nil {
		t.Fatal(err)
	}
	if runStatus != "" {
		if _, err := s.DB.ExecContext(ctx, "UPDATE runs SET status=? WHERE id=?", runStatus, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	execute := func(context.Context, store.RunLease, store.QueueEntry) (store.Run, error) {
		t.Error("a run past its delivery limit was executed")
		return store.Run{}, errors.New("must not run")
	}
	w := New(s, execute, Config{ID: "worker-a", LeaseTTL: time.Minute, MaxAttempts: 1, Clock: func() time.Time { return base }, Logger: quietLogger()})
	observed := &[]Outcome{}
	w.Observer = func(o Outcome) { *observed = append(*observed, o) }
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	return w, run, observed
}

// A claim settled without executing it was observed with a zero duration, so each
// one put a 0 s run in the histogram's smallest bucket and added nothing to the
// run-seconds counter although the worker had spent the time reading and writing
// the run.
func TestOverDeliveredClaimIsTimed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		runStatus string
		want      Disposition
	}{
		{"a run that had already completed is finished", "completed", Finished},
		{"a run that never completed is failed", "", Failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := openStore(ctx, t)
			w, _, observed := overDeliveredWorker(ctx, t, s, tc.runStatus)
			outcome, err := w.ClaimAndRun(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Disposition != tc.want {
				t.Fatalf("outcome=%+v want disposition %s", outcome, tc.want)
			}
			if outcome.Duration <= 0 {
				t.Fatalf("duration=%v, want the time the settlement took", outcome.Duration)
			}
			if len(*observed) != 1 || (*observed)[0].Duration != outcome.Duration {
				t.Fatalf("the observer saw %+v, want the returned outcome with its duration", *observed)
			}
		})
	}
}

// When the run cannot be read, the claim is not settled, and the caller gets the
// error. Observing it anyway counted the claim without a disposition, so
// claims_total rose and no disposition series did.
func TestOverDeliveredClaimWithAnUnreadableRunIsNotObserved(t *testing.T) {
	ctx := context.Background()
	s := openStore(ctx, t)
	w, _, observed := overDeliveredWorker(ctx, t, s, "")
	// The claim itself does not read this column, so it succeeds and the read of the
	// run that follows does not.
	if _, err := s.DB.ExecContext(ctx, "ALTER TABLE runs DROP COLUMN transcript"); err != nil {
		t.Fatal(err)
	}
	outcome, err := w.ClaimAndRun(ctx)
	if err == nil || !strings.Contains(err.Error(), "read run past delivery limit") {
		t.Fatalf("err=%v, want the failure to read the run (outcome %+v)", err, outcome)
	}
	if len(*observed) != 0 {
		t.Fatalf("the observer saw %+v for a claim that ended in no disposition", *observed)
	}
}

// A closing write the fence refuses means another worker holds the run now. That
// is the fenced disposition, with a fencing rejection counted and recorded, and
// not a finished run: the queue entry was not finished by this worker.
func TestClaimRefusedByTheFenceAtTheEndIsFenced(t *testing.T) {
	ctx := context.Background()
	s := openStore(ctx, t)
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	run := queuedRunForTiming(ctx, t, s, base)
	// The run ends normally, but a newer owner took the lease during it. The
	// renewal has not ticked (a minute's lease renews every twenty seconds), so
	// the first the worker learns of it is the write that finishes the run.
	execute := func(ctx context.Context, lease store.RunLease, _ store.QueueEntry) (store.Run, error) {
		_, err := s.DB.ExecContext(ctx, "UPDATE run_leases SET owner=?, fence=fence+1 WHERE run_id=?", "worker-b", lease.RunID)
		return store.Run{ID: lease.RunID, Status: "completed"}, err
	}
	w := New(s, execute, Config{ID: "worker-a", LeaseTTL: time.Minute, Clock: func() time.Time { return base }, Logger: quietLogger()})
	var observed []Outcome
	w.Observer = func(o Outcome) { observed = append(observed, o) }
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatalf("a fenced claim is an outcome, not an error: %v", err)
	}
	if outcome.Disposition != Fenced || outcome.Err == "" {
		t.Fatalf("outcome=%+v want a fenced disposition with the refusal in Err", outcome)
	}
	if len(observed) != 1 || observed[0].Disposition != Fenced {
		t.Fatalf("the observer saw %+v, want one fenced outcome", observed)
	}
	entry, err := s.QueueEntryFor(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.State == store.QueueFinished {
		t.Fatalf("the fenced worker finished the queue entry: %+v", entry)
	}
	history, err := s.Ownership(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rejected := 0
	for _, event := range history {
		if event.Kind == store.OwnershipFenced && event.Owner == "worker-a" {
			rejected++
		}
	}
	if rejected != 1 {
		t.Fatalf("ownership history has %d fence rejections by worker-a, want 1: %+v", rejected, history)
	}
}

// A closing write that fails for any other reason leaves the claim unsettled: the
// lease lapses and the run is delivered again. The claim did not end as finished,
// so it is not observed as finished; the caller gets the error.
func TestClaimWhoseClosingWriteFailsIsNotObservedAsFinished(t *testing.T) {
	ctx := context.Background()
	s := openStore(ctx, t)
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	queuedRunForTiming(ctx, t, s, base)
	execute := func(ctx context.Context, lease store.RunLease, _ store.QueueEntry) (store.Run, error) {
		_, err := s.DB.ExecContext(ctx, "DROP TABLE work_queue")
		return store.Run{ID: lease.RunID, Status: "completed"}, err
	}
	w := New(s, execute, Config{ID: "worker-a", LeaseTTL: time.Minute, Clock: func() time.Time { return base }, Logger: quietLogger()})
	var observed []Outcome
	w.Observer = func(o Outcome) { observed = append(observed, o) }
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ClaimAndRun(ctx); err == nil || errors.Is(err, store.ErrFenced) {
		t.Fatalf("err=%v, want the store's failure to close the claim", err)
	}
	if len(observed) != 0 {
		t.Fatalf("the observer saw %+v for a claim that was never closed", observed)
	}
}
