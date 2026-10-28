package dispatch

import (
	"context"
	"errors"
	"testing"

	"twinwright/internal/crashpoint"
	"twinwright/internal/store"
)

// refundArgs refunds the whole duplicate charge; its amount depends on the
// world's seed, so it is read rather than assumed.
func refundArgs(t *testing.T, s *store.Store, run store.Run) map[string]any {
	t.Helper()
	var amount int64
	if err := s.DB.QueryRow("SELECT amount_cents FROM charges WHERE world_id=? AND id='CH-1002'", run.WorldID).Scan(&amount); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"charge_id": "CH-1002", "amount_cents": amount, "reason": "duplicate charge"}
}

func crashOn(point crashpoint.Point, sites *[]crashpoint.Site) crashpoint.Hook {
	return func(_ context.Context, site crashpoint.Site) error {
		*sites = append(*sites, site)
		if site.Point == point {
			return errors.New("test crash")
		}
		return nil
	}
}

func counts(t *testing.T, s *store.Store, run store.Run) (events, results, refunds int) {
	t.Helper()
	for query, dest := range map[string]*int{
		"SELECT count(*) FROM events WHERE run_id=?":       &events,
		"SELECT count(*) FROM tool_results WHERE run_id=?": &results,
	} {
		if err := s.DB.QueryRow(query, run.ID).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DB.QueryRow("SELECT count(*) FROM refunds WHERE world_id=?", run.WorldID).Scan(&refunds); err != nil {
		t.Fatal(err)
	}
	return events, results, refunds
}

func TestCrashBeforeToolCommitLeavesNothing(t *testing.T) {
	d, s, run := setup(t, "")
	eventsBefore, _, _ := counts(t, s, run)
	args := refundArgs(t, s, run)
	var sites []crashpoint.Site
	d.Crash = crashOn(crashpoint.BeforeToolCommit, &sites)
	_, err := d.Invoke(context.Background(), run.ID, "refund-1", "createRefund", args)
	if !errors.Is(err, crashpoint.ErrCrashed) {
		t.Fatalf("error=%v, want ErrCrashed", err)
	}
	if len(sites) != 1 || sites[0].Point != crashpoint.BeforeToolCommit || sites[0].RunID != run.ID ||
		sites[0].CallID != "refund-1" || sites[0].OperationID != "createRefund" {
		t.Fatalf("sites=%+v", sites)
	}
	events, results, refunds := counts(t, s, run)
	if events != eventsBefore || results != 0 || refunds != 0 {
		t.Fatalf("events %d->%d tool_results=%d refunds=%d, want nothing left behind", eventsBefore, events, results, refunds)
	}
}

func TestCrashAfterToolCommitIsAnsweredFromTheCacheOnRetry(t *testing.T) {
	d, s, run := setup(t, "")
	args := refundArgs(t, s, run)
	var sites []crashpoint.Site
	d.Crash = crashOn(crashpoint.AfterToolCommit, &sites)
	_, err := d.Invoke(context.Background(), run.ID, "refund-1", "createRefund", args)
	if !errors.Is(err, crashpoint.ErrCrashed) {
		t.Fatalf("error=%v, want ErrCrashed", err)
	}
	if len(sites) != 2 || sites[0].Point != crashpoint.BeforeToolCommit || sites[1].Point != crashpoint.AfterToolCommit ||
		sites[1].CallID != "refund-1" || sites[1].OperationID != "createRefund" {
		t.Fatalf("sites=%+v", sites)
	}
	events, results, refunds := counts(t, s, run)
	if results != 1 || refunds != 1 {
		t.Fatalf("tool_results=%d refunds=%d, want the committed effect", results, refunds)
	}
	var saved string
	if err = s.DB.QueryRow("SELECT body FROM tool_results WHERE run_id=? AND call_id='refund-1'", run.ID).Scan(&saved); err != nil {
		t.Fatal(err)
	}

	sites = nil
	d.Crash = func(_ context.Context, site crashpoint.Site) error {
		sites = append(sites, site)
		return nil
	}
	result, err := d.Invoke(context.Background(), run.ID, "refund-1", "createRefund", args)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != 201 || string(result.Body) != saved {
		t.Fatalf("retry=%+v, want saved body %s", result, saved)
	}
	if len(sites) != 0 {
		t.Fatalf("idempotency-cache hit fired crash points: %+v", sites)
	}
	eventsAfter, resultsAfter, refundsAfter := counts(t, s, run)
	if eventsAfter != events || resultsAfter != 1 || refundsAfter != 1 {
		t.Fatalf("retry changed state: events %d->%d tool_results=%d refunds=%d", events, eventsAfter, resultsAfter, refundsAfter)
	}
}
