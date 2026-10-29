package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"twinwright/internal/store"
)

func workerIDs(ctx context.Context, t *testing.T, s *store.Store) map[string]store.WorkerRecord {
	t.Helper()
	workers, err := s.Workers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.WorkerRecord{}
	for _, record := range workers {
		out[record.ID] = record
	}
	return out
}

// A worker that stops cleanly takes its row out, or the fleet table and the
// metrics built on it grow by one row for every restart. Close is the end of a
// worker's life, and Serve and the worker command both end with it.
func TestCloseRemovesTheWorkersRow(t *testing.T) {
	ctx := context.Background()
	s := openStore(ctx, t)
	execute := func(context.Context, store.RunLease, store.QueueEntry) (store.Run, error) { return store.Run{}, nil }
	w := New(s, execute, Config{ID: "worker-a", LeaseTTL: time.Minute, Logger: quietLogger()})
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := workerIDs(ctx, t, s)["worker-a"]; !ok {
		t.Fatal("the worker did not register")
	}
	w.Close()
	if _, ok := workerIDs(ctx, t, s)["worker-a"]; ok {
		t.Fatal("the worker's row is still there after a clean stop")
	}
	w.Close() // a second call is harmless
}

// A claim that could not be handed back leaves a live lease. The row says who
// holds it, so it stays until the lease lapses.
func TestCloseKeepsTheRowOfAWorkerThatStillOwnsALiveLease(t *testing.T) {
	ctx := context.Background()
	s := openStore(ctx, t)
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	queuedRunForTiming(ctx, t, s, base)
	clock := func() time.Time { return base }
	execute := func(context.Context, store.RunLease, store.QueueEntry) (store.Run, error) { return store.Run{}, nil }
	w := New(s, execute, Config{ID: "worker-a", LeaseTTL: time.Hour, Clock: clock, Logger: quietLogger()})
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimRun(ctx, "worker-a", time.Hour, base); err != nil {
		t.Fatal(err)
	}
	w.Close()
	if _, ok := workerIDs(ctx, t, s)["worker-a"]; !ok {
		t.Fatal("Close removed the row of a worker that owns a live lease")
	}
}

// Serve is the long-running path: a stop request ends it, and the row goes with it.
func TestServeRemovesItsRowOnACleanStop(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	s := openStore(ctx, t)
	execute := func(context.Context, store.RunLease, store.QueueEntry) (store.Run, error) { return store.Run{}, nil }
	w := New(s, execute, Config{ID: "worker-a", LeaseTTL: time.Minute, PollInterval: 5 * time.Millisecond, Logger: quietLogger()})
	done := make(chan error, 1)
	go func() { done <- w.Serve(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := workerIDs(context.Background(), t, s)["worker-a"]; ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Serve never registered the worker")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve=%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop")
	}
	if _, ok := workerIDs(context.Background(), t, s)["worker-a"]; ok {
		t.Fatal("Serve left the worker's row behind after a clean stop")
	}
}

// last_seen_at moves on register, claim and renewal, none of which happen while a
// worker waits for work. A metrics window over last_seen_at would then drop a
// healthy idle worker, so a poll that finds nothing refreshes the row, at most
// once per IdleHeartbeatEvery.
func TestAnIdleWorkerStaysSeen(t *testing.T) {
	ctx := context.Background()
	s := openStore(ctx, t)
	clock := newClock()
	execute := func(context.Context, store.RunLease, store.QueueEntry) (store.Run, error) { return store.Run{}, nil }
	w := New(s, execute, Config{ID: "worker-a", LeaseTTL: time.Minute, Clock: clock.Clock(), Logger: quietLogger()})
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	registeredAt := store.CoordinationTime(clock.Now())
	poll := func() string {
		t.Helper()
		if _, err := w.ClaimAndRun(ctx); !errors.Is(err, store.ErrNoWork) {
			t.Fatalf("poll err=%v, want no work", err)
		}
		return workerIDs(ctx, t, s)["worker-a"].LastSeenAt
	}

	// The first idle poll refreshes at once; the next ones inside the interval do not.
	clock.Advance(time.Second)
	first := poll()
	if first != store.CoordinationTime(clock.Now()) || first == registeredAt {
		t.Fatalf("after the first idle poll last_seen_at=%q, want %q", first, store.CoordinationTime(clock.Now()))
	}
	clock.Advance(IdleHeartbeatEvery - 2*time.Second)
	if got := poll(); got != first {
		t.Fatalf("an idle poll inside the interval wrote last_seen_at=%q, want it left at %q", got, first)
	}
	clock.Advance(3 * time.Second)
	if got := poll(); got != store.CoordinationTime(clock.Now()) {
		t.Fatalf("after %v idle last_seen_at=%q, want %q", IdleHeartbeatEvery, got, store.CoordinationTime(clock.Now()))
	}
}
