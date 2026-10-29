package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// The tests below need a PostgreSQL server (TWINWRIGHT_TEST_POSTGRES_DSN) because
// the defects they pin only exist under concurrent connections. SQLite has one
// writer, so the same code cannot race there.

// TestConcurrentFirstAcquireYieldsOneHolderPostgres: two claimers that reach a
// never-leased run together both read "no lease row", so both computed fence 1.
// The second INSERT then waited for the first to commit and took the DO UPDATE
// branch, replacing a live owner without ErrLeaseHeld and giving two workers the
// same fence. Writes stayed safe because the displaced holder fails its first
// guarded write, but "every acquisition raises the fence" and "a live lease is
// refused" were both untrue in that window.
func TestConcurrentFirstAcquireYieldsOneHolderPostgres(t *testing.T) {
	ctx := context.Background()
	s := openTestPostgres(ctx, t)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	// Many rounds, each a fresh never-leased run: the race is a window of a few
	// milliseconds, and one clean round proves nothing.
	const rounds, contenders = 30, 8
	for round := 0; round < rounds; round++ {
		run := seededRun(ctx, t, s)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		var winners []RunLease
		var unexpected []error
		for i := 0; i < contenders; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				<-start
				lease, err := s.AcquireRunLease(ctx, run.ID, fmt.Sprintf("worker-%d", n), time.Minute, now)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					winners = append(winners, lease)
				case !errors.Is(err, ErrLeaseHeld):
					unexpected = append(unexpected, err)
				}
			}(i)
		}
		close(start)
		wg.Wait()

		if len(unexpected) > 0 {
			t.Fatalf("round %d: acquire failed for a reason other than contention: %v", round, unexpected)
		}
		if len(winners) != 1 || winners[0].Token != 1 {
			t.Fatalf("round %d: %d workers acquired a never-leased run (%+v), want exactly one at fence 1", round, len(winners), winners)
		}
		held, err := s.RunLease(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if held.Owner != winners[0].Owner || held.Token != 1 {
			t.Fatalf("round %d: lease row says %q at fence %d, but %q won at fence 1", round, held.Owner, held.Token, winners[0].Owner)
		}
		history, err := s.Ownership(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(history) != 1 || history[0].Kind != OwnershipAcquired {
			t.Fatalf("round %d: ownership log %+v, want one acquisition", round, history)
		}
	}
}

// TestConcurrentOwnershipAppendsNeverCollidePostgres: the ownership log picked its
// next sequence number with MAX(seq)+1 and no lock, so two appenders that read the
// same maximum collided on the primary key and the later one failed. A stale
// worker recording its fence rejection (which takes no lock) could thereby fail a
// legitimate owner's claim or release.
func TestConcurrentOwnershipAppendsNeverCollidePostgres(t *testing.T) {
	ctx := context.Background()
	s := openTestPostgres(ctx, t)
	run := seededRun(ctx, t, s)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	lease, err := s.AcquireRunLease(ctx, run.ID, "owner", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	stale := lease.Fence()

	const appenders, perAppender, cycles = 6, 15, 15
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []error
	record := func(err error) {
		if err != nil {
			mu.Lock()
			failures = append(failures, err)
			mu.Unlock()
		}
	}
	for i := 0; i < appenders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < perAppender; j++ {
				record(s.RecordFenceRejection(ctx, stale, now, "stale"))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		current := lease
		for j := 0; j < cycles; j++ {
			if err := s.ReleaseRunLease(ctx, current, now); err != nil {
				record(fmt.Errorf("release: %w", err))
				return
			}
			next, err := s.AcquireRunLease(ctx, run.ID, "owner", time.Hour, now)
			if err != nil {
				record(fmt.Errorf("acquire: %w", err))
				return
			}
			current = next
		}
	}()
	close(start)
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("%d ownership writes failed, first: %v", len(failures), failures[0])
	}
	history, err := s.Ownership(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := 1 + appenders*perAppender + cycles*2
	if len(history) != want {
		t.Fatalf("ownership log has %d rows, want %d", len(history), want)
	}
	for i, event := range history {
		if event.Seq != i+1 {
			t.Fatalf("row %d has seq %d: the log must run 1..n without gaps", i, event.Seq)
		}
	}
}
