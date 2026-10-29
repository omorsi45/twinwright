package worker

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"twinwright/internal/store"
)

// syncBuffer is a log sink a test can read while a worker is still writing to it.
type syncBuffer struct {
	mu   sync.Mutex
	text strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.Write(p)
}

func (b *syncBuffer) count(substr string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.text.String(), substr)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.String()
}

// loggerTo logs warnings and errors to w, which is what an operator sees from a
// worker whose renewal is failing.
func loggerTo(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// TestOneSecondLeaseRenewsAtLeastTwiceDuringALongerRun is the case the store's
// own minimum allows. RenewEvery used to be floored at one second, so with a one
// second TTL the first renewal ran at the moment the lease expired and was
// refused: every run longer than the TTL was fenced out by its own worker. It
// runs on a real clock, because a renewal that lands exactly at expiry is a
// timing property a manual clock cannot show.
func TestOneSecondLeaseRenewsAtLeastTwiceDuringALongerRun(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var renewals atomic.Int32
	hold := func(execCtx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		select {
		case <-execCtx.Done():
			return store.Run{}, execCtx.Err()
		case <-time.After(2500 * time.Millisecond):
		}
		return s.Run(ctx, lease.RunID)
	}
	w := New(s, hold, Config{ID: "worker-a", LeaseTTL: time.Second, Logger: quietLogger(), OnRenew: func(store.RunLease) { renewals.Add(1) }})
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != Finished {
		t.Fatalf("a 2.5 s run under a 1 s lease ended %s (%s) with RenewEvery %s", outcome.Disposition, outcome.Err, w.Config.RenewEvery)
	}
	if got := renewals.Load(); got < 2 {
		t.Fatalf("renewals=%d during a 2.5 s run under a 1 s lease, want at least 2 (RenewEvery %s)", got, w.Config.RenewEvery)
	}
}

// TestRenewalCadenceLeavesMarginAtTheMinimumTTL: whatever the TTL, the default
// interval leaves at least one retry before the lease expires, and the floor that
// protects a very short TTL from a busy loop is never allowed to reach it.
func TestRenewalCadenceLeavesMarginAtTheMinimumTTL(t *testing.T) {
	for _, ttl := range []time.Duration{time.Second, 1500 * time.Millisecond, 3 * time.Second, DefaultLeaseTTL, 10 * time.Minute} {
		got := Config{LeaseTTL: ttl}.withDefaults().RenewEvery
		if got > ttl/2 {
			t.Errorf("LeaseTTL %s: RenewEvery %s leaves less than half the lease for a retry", ttl, got)
		}
		if got < 100*time.Millisecond {
			t.Errorf("LeaseTTL %s: RenewEvery %s is below the 100ms floor", ttl, got)
		}
	}
	// A TTL too short for the floor to fit twice keeps the proportion instead.
	if got := (Config{LeaseTTL: 150 * time.Millisecond}).withDefaults().RenewEvery; got != 50*time.Millisecond {
		t.Errorf("LeaseTTL 150ms: RenewEvery %s want 50ms (a third), not the floor", got)
	}
	// An explicit interval is left alone.
	if got := (Config{LeaseTTL: time.Minute, RenewEvery: 10 * time.Millisecond}).withDefaults().RenewEvery; got != 10*time.Millisecond {
		t.Errorf("an explicit RenewEvery was changed to %s", got)
	}
}

// busyStore opens a SQLite store whose connection fails at once, instead of
// waiting out the five second busy timeout, when another connection holds the
// write lock. A renewal then fails the way a database blip does: with an error
// that says nothing about who owns the run.
func busyStore(ctx context.Context, t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "busy.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err = s.DB.ExecContext(ctx, "PRAGMA busy_timeout=0"); err != nil {
		t.Fatal(err)
	}
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	return s, other
}

// holdWriteLock takes SQLite's write lock on a second connection and returns the
// function that gives it up.
func holdWriteLock(ctx context.Context, t *testing.T, other *sql.DB) func() {
	t.Helper()
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Error(err)
		return func() {}
	}
	if _, err = conn.ExecContext(ctx, "PRAGMA busy_timeout=2000"); err != nil {
		t.Error(err)
	}
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Errorf("take the write lock: %v", err)
		conn.Close()
		return func() {}
	}
	return func() {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		conn.Close()
	}
}

func waitFor(t *testing.T, what string, within time.Duration, done <-chan struct{}, condition func() bool) bool {
	t.Helper()
	deadline := time.After(within)
	for !condition() {
		select {
		case <-done:
			return false
		case <-deadline:
			t.Errorf("timed out waiting for %s", what)
			return false
		case <-time.After(5 * time.Millisecond):
		}
	}
	return true
}

func ownershipKinds(ctx context.Context, t *testing.T, s *store.Store, runID string) []string {
	t.Helper()
	history, err := s.Ownership(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(history))
	for _, event := range history {
		kinds = append(kinds, event.Kind)
	}
	return kinds
}

// TestTransientRenewalErrorDoesNotAbandonTheRun: a renewal that fails for a
// reason other than a lost lease (here another connection holds SQLite's write
// lock) used to cancel the run, report the claim as fenced and write a
// fence.rejected row, although nobody took the run and the lease was still this
// worker's. It now logs, tries again at the next interval while the lease is
// still valid by the worker's own clock, and carries on once the database
// answers.
func TestTransientRenewalErrorDoesNotAbandonTheRun(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s, other := busyStore(ctx, t)
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	var renewals atomic.Int32
	execute := func(execCtx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		release := holdWriteLock(ctx, t, other)
		failed := waitFor(t, "three failed renewals", 10*time.Second, execCtx.Done(), func() bool { return logs.count("lease renewal failed") >= 3 })
		release()
		if !failed {
			return store.Run{}, execCtx.Err()
		}
		waitFor(t, "a renewal after the lock was released", 10*time.Second, execCtx.Done(), func() bool { return renewals.Load() >= 1 })
		if execCtx.Err() != nil {
			return store.Run{}, execCtx.Err()
		}
		return s.Run(ctx, lease.RunID)
	}
	w := New(s, execute, Config{ID: "worker-a", LeaseTTL: time.Minute, RenewEvery: 50 * time.Millisecond,
		Logger: loggerTo(&logs), OnRenew: func(store.RunLease) { renewals.Add(1) }})
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != Finished {
		t.Fatalf("a blip in the database ended the claim as %s (%s)\n%s", outcome.Disposition, outcome.Err, logs.String())
	}
	for _, kind := range ownershipKinds(ctx, t, s, run.ID) {
		if kind == store.OwnershipFenced {
			t.Fatal("a transient renewal error wrote a fence.rejected row")
		}
	}
	if renewals.Load() < 1 {
		t.Fatal("renewals did not resume once the database answered")
	}
}

// TestRenewalErrorsPastTheLocalExpiryAbandonTheRun is the other half: retrying is
// only safe while the lease is still valid by the worker's own clock. Once its
// expiry has passed another worker may have taken the run, so the worker stops.
// Nobody rejected a fence, so no fence.rejected row is written, and the reason
// is the renewal error rather than the cancellation it caused.
func TestRenewalErrorsPastTheLocalExpiryAbandonTheRun(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s, other := busyStore(ctx, t)
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	execute := func(execCtx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		release := holdWriteLock(ctx, t, other)
		defer release()
		select {
		case <-execCtx.Done():
			return store.Run{}, execCtx.Err()
		case <-time.After(10 * time.Second):
			return store.Run{}, errors.New("execution was never cancelled after the lease expired")
		}
	}
	w := New(s, execute, Config{ID: "worker-a", LeaseTTL: time.Second, Logger: loggerTo(&logs)})
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != Fenced {
		t.Fatalf("outcome=%+v want the claim abandoned once the lease expired", outcome)
	}
	if !strings.Contains(outcome.Err, "expired") || !strings.Contains(outcome.Err, "locked") {
		t.Fatalf("outcome error %q should say the lease expired and carry the renewal error", outcome.Err)
	}
	if logs.count("trying again at the next interval") < 1 {
		t.Fatalf("the worker did not retry before giving up:\n%s", logs.String())
	}
	for _, kind := range ownershipKinds(ctx, t, s, run.ID) {
		if kind == store.OwnershipFenced {
			t.Fatal("an expiry the worker noticed itself was recorded as a fence rejection")
		}
	}
}

// TestAbandonedRunRecordsTheRenewalCause: when the lease really is lost, the
// audit row says what the store refused, not that a context was cancelled.
func TestAbandonedRunRecordsTheRenewalCause(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	stolen := func(execCtx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		clock.Advance(2 * time.Minute)
		if _, err := s.AcquireRunLease(ctx, lease.RunID, "worker-b", time.Minute, clock.Now()); err != nil {
			return store.Run{}, err
		}
		select {
		case <-execCtx.Done():
			return store.Run{}, execCtx.Err()
		case <-time.After(10 * time.Second):
			return store.Run{}, errors.New("execution was never cancelled after the lease was lost")
		}
	}
	w := New(s, stolen, Config{ID: "worker-a", LeaseTTL: time.Minute, RenewEvery: 50 * time.Millisecond, Clock: clock.Clock(), Logger: quietLogger()})
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != Fenced {
		t.Fatalf("outcome=%+v want fenced", outcome)
	}
	history, err := s.Ownership(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var detail string
	for _, event := range history {
		if event.Kind == store.OwnershipFenced {
			detail = string(event.Detail)
		}
	}
	if detail == "" {
		t.Fatal("losing the lease was not recorded")
	}
	if strings.Contains(detail, "context canceled") || !strings.Contains(detail, "renew rejected") {
		t.Fatalf("the audit row should carry the store's refusal, not the cancellation: %s", detail)
	}
	if !strings.Contains(outcome.Err, "renew rejected") {
		t.Fatalf("outcome error %q should carry the store's refusal", outcome.Err)
	}
}

// TestRenewEveryAtOrAboveTheTTLIsRefused: an interval as long as the lease means
// the first renewal runs at expiry and is refused, so a run longer than the TTL
// would fence itself out. Nothing sensible can be done with such a configuration,
// so it is an error, reported before anything is claimed or leased.
func TestRenewEveryAtOrAboveTheTTLIsRefused(t *testing.T) {
	for _, renewEvery := range []time.Duration{time.Second, 2 * time.Second} {
		t.Run(renewEvery.String(), func(t *testing.T) {
			ctx := context.Background()
			manifest := billingManifest(t)
			s := openStore(ctx, t)
			run := seedRun(ctx, t, s, manifest, "")
			if err := s.Enqueue(ctx, run.ID, 10, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			cfg := Config{ID: "worker-a", LeaseTTL: time.Second, RenewEvery: renewEvery, Logger: quietLogger()}
			var executed atomic.Int32
			execute := func(context.Context, store.RunLease, store.QueueEntry) (store.Run, error) {
				executed.Add(1)
				return store.Run{}, nil
			}
			wantText := "renew interval " + renewEvery.String() + " must be shorter than the lease TTL 1s"

			w := New(s, execute, cfg)
			if _, err := w.ClaimAndRun(ctx); err == nil || !strings.Contains(err.Error(), wantText) {
				t.Fatalf("ClaimAndRun err=%v, want it to say %q", err, wantText)
			}
			entry, err := s.QueueEntryFor(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if entry.State != store.QueueRunnable || entry.Attempts != 0 {
				t.Fatalf("a refused configuration still claimed the run: %+v", entry)
			}
			if err = w.Serve(ctx); err == nil || !strings.Contains(err.Error(), wantText) {
				t.Fatalf("Serve err=%v, want it to say %q", err, wantText)
			}

			_, err = ExecuteLocal(ctx, s, run.ID, cfg, func(context.Context, store.Fence) (store.Run, error) {
				executed.Add(1)
				return store.Run{}, nil
			})
			if err == nil || !strings.Contains(err.Error(), wantText) {
				t.Fatalf("ExecuteLocal err=%v, want it to say %q", err, wantText)
			}
			if _, err = s.RunLease(ctx, run.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("ExecuteLocal took a lease before refusing the configuration: %v", err)
			}
			if executed.Load() != 0 {
				t.Fatalf("work ran %d times under a refused configuration", executed.Load())
			}
		})
	}
}

func TestConfigValidateRefusesARenewalIntervalThatReachesTheTTL(t *testing.T) {
	for _, cfg := range []Config{
		{},
		{LeaseTTL: time.Second},
		{LeaseTTL: time.Minute, RenewEvery: 10 * time.Millisecond},
		{LeaseTTL: time.Second, RenewEvery: 999 * time.Millisecond},
	} {
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(%+v): %v", cfg, err)
		}
	}
	for _, cfg := range []Config{
		{LeaseTTL: time.Second, RenewEvery: time.Second},
		{LeaseTTL: 5 * time.Second, RenewEvery: time.Minute},
		// The default TTL is what an unset LeaseTTL is compared against.
		{RenewEvery: time.Minute},
	} {
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate(%+v) accepted a renewal interval at or above the TTL", cfg)
		}
	}
}
