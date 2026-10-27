package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

func TestExecuteLocalFencesItsWrites(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	run := seedRun(ctx, t, s, manifest, "")

	called := false
	_, err := ExecuteLocal(ctx, s, run.ID, Config{Logger: quietLogger()}, func(ctx context.Context, fence store.Fence) (store.Run, error) {
		called = true
		if fence.Owner != LocalOwner() || fence.Token != 1 || fence.RunID != run.ID {
			t.Errorf("fence=%+v", fence)
		}
		d := &dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &fence}
		if _, err := d.Invoke(ctx, run.ID, "call-local-1", "getCustomer", map[string]any{"id": "C-1001"}); err != nil {
			return store.Run{}, err
		}
		return s.Run(ctx, run.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("fn was not called")
	}
	lease, err := s.RunLease(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Owner != "" {
		t.Fatalf("lease still owned by %q after ExecuteLocal returned", lease.Owner)
	}
	if lease.Token != 1 {
		t.Fatalf("fence token=%d want 1", lease.Token)
	}
}

func TestExecuteLocalRefusesARunLeasedByAFleetWorker(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	held, err := s.RunLease(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	called := false
	_, err = ExecuteLocal(ctx, s, run.ID, Config{Logger: quietLogger()}, func(context.Context, store.Fence) (store.Run, error) {
		called = true
		return store.Run{}, nil
	})
	if !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("err=%v want ErrLeaseHeld", err)
	}
	if !strings.Contains(err.Error(), "worker-a") {
		t.Fatalf("error does not name the holder: %v", err)
	}
	wantExpiry := held.ExpiresAt.UTC().Format(time.RFC3339Nano)
	if !strings.Contains(err.Error(), wantExpiry) {
		t.Fatalf("error does not name the expiry %s: %v", wantExpiry, err)
	}
	if called {
		t.Fatal("fn ran against a run another worker holds")
	}
}

func TestFleetCannotClaimARunExecutingLocally(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	called := false
	_, err := ExecuteLocal(ctx, s, run.ID, Config{Logger: quietLogger()}, func(ctx context.Context, fence store.Fence) (store.Run, error) {
		called = true
		if _, _, err := s.ClaimRun(ctx, "worker-b", time.Minute, time.Now().UTC()); !errors.Is(err, store.ErrNoWork) {
			t.Errorf("fleet claim during local execution err=%v want ErrNoWork", err)
		}
		return store.Run{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("fn was not called")
	}
}

// TestExecuteLocalRenewsItsLease drives lease time by hand so the verdict does
// not depend on how long the host stalls. A real-time version (2s TTL, 1s
// renewals, a 3s sleep) had a 1s margin, and a stall longer than that let the
// lease lapse; rejecting a lapsed renewal is correct, so the test failed for a
// reason unrelated to renewal.
//
// Only the renewal cadence is real (10ms ticks). Each round advances the
// manual clock by 1s, half the TTL, and waits until the stored expiry is more
// than 1s past the clock. Without a renewal after the advance the expiry stays
// at most 1s ahead, so seeing it move can only mean the loop renewed. Six
// rounds cover 6s, three times the TTL, so the final guard passes only if
// every round extended the lease.
func TestExecuteLocalRenewsItsLease(t *testing.T) {
	const (
		rounds    = 6
		step      = time.Second
		pollEvery = 2 * time.Millisecond
		deadline  = 10 * time.Second
	)
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	run := seedRun(ctx, t, s, manifest, "")

	clock := newClock()
	cfg := Config{LeaseTTL: 2 * time.Second, RenewEvery: 10 * time.Millisecond, Clock: clock.Clock(), Logger: quietLogger()}
	_, err := ExecuteLocal(ctx, s, run.ID, cfg, func(ctx context.Context, fence store.Fence) (store.Run, error) {
		for round := 1; round <= rounds; round++ {
			clock.Advance(step)
			giveUp := time.Now().Add(deadline)
			for {
				lease, err := s.RunLease(ctx, run.ID)
				if err != nil {
					return store.Run{}, err
				}
				if lease.ExpiresAt.After(clock.Now().Add(step)) {
					break
				}
				if time.Now().After(giveUp) {
					return store.Run{}, fmt.Errorf("round %d: lease expiry %s was not extended past %s within %s of real time: the renewal loop did not renew",
						round, lease.ExpiresAt, clock.Now().Add(step), deadline)
				}
				select {
				case <-ctx.Done():
					return store.Run{}, ctx.Err()
				case <-time.After(pollEvery):
				}
			}
		}
		if err := s.GuardFence(ctx, fence, clock.Now()); err != nil {
			return store.Run{}, err
		}
		return store.Run{}, nil
	})
	if err != nil {
		t.Fatalf("lease was not renewed: %v", err)
	}
}

func TestExecuteLocalReleasesTheLeaseWhenCancelled(t *testing.T) {
	manifest := billingManifest(t)
	s := openStore(context.Background(), t)
	run := seedRun(context.Background(), t, s, manifest, "")

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := ExecuteLocal(parent, s, run.ID, Config{Logger: quietLogger()}, func(ctx context.Context, fence store.Fence) (store.Run, error) {
		cancel()
		<-ctx.Done()
		return store.Run{}, ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want context.Canceled", err)
	}
	lease, err := s.RunLease(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Owner != "" {
		t.Fatalf("cancelled run is still leased by %q", lease.Owner)
	}
}

// TestExecuteLocalReportsAFailedReleaseAfterCancellation breaks the store so
// the release cannot reach it. Cancellation is the Ctrl-C path, the case the
// release exists for, and a release that fails there leaves the run locked
// until the TTL lapses. The caller has to see that, next to the cancellation,
// or the next resume reports a lease held by a process that has already gone.
func TestExecuteLocalReportsAFailedReleaseAfterCancellation(t *testing.T) {
	manifest := billingManifest(t)
	s := openStore(context.Background(), t)
	run := seedRun(context.Background(), t, s, manifest, "")

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := ExecuteLocal(parent, s, run.ID, Config{Logger: quietLogger()}, func(ctx context.Context, fence store.Fence) (store.Run, error) {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
		cancel()
		<-ctx.Done()
		return store.Run{}, ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want context.Canceled kept", err)
	}
	if err == nil || !strings.Contains(err.Error(), "release lease on run "+run.ID) || !strings.Contains(err.Error(), "database is closed") {
		t.Fatalf("err=%v does not report the failed release", err)
	}
}

// TestExecuteLocalReportsAFailedReleaseOfACompletedRun covers the success
// path: the run finished, so the error must say the lease was left behind and
// must not say the run failed.
func TestExecuteLocalReportsAFailedReleaseOfACompletedRun(t *testing.T) {
	manifest := billingManifest(t)
	s := openStore(context.Background(), t)
	run := seedRun(context.Background(), t, s, manifest, "")

	_, err := ExecuteLocal(context.Background(), s, run.ID, Config{Logger: quietLogger()}, func(context.Context, store.Fence) (store.Run, error) {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
		return store.Run{ID: run.ID, Status: "completed"}, nil
	})
	if !errors.Is(err, ErrLeaseNotReleased) {
		t.Fatalf("err=%v want ErrLeaseNotReleased", err)
	}
	if !strings.Contains(err.Error(), "database is closed") || strings.Contains(err.Error(), "failed") {
		t.Fatalf("err=%q should carry the cause and not claim the run failed", err)
	}
}

// TestExecuteLocalDoesNotCallAPausedRunCompleted covers the same path for a run
// that stopped at its step budget. fn returned no error, but the run is paused,
// so the message about the lease left behind must say what status the run
// reached and not that it completed.
func TestExecuteLocalDoesNotCallAPausedRunCompleted(t *testing.T) {
	manifest := billingManifest(t)
	s := openStore(context.Background(), t)
	run := seedRun(context.Background(), t, s, manifest, "")

	_, err := ExecuteLocal(context.Background(), s, run.ID, Config{Logger: quietLogger()}, func(context.Context, store.Fence) (store.Run, error) {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
		return store.Run{ID: run.ID, Status: "paused"}, nil
	})
	if !errors.Is(err, ErrLeaseNotReleased) {
		t.Fatalf("err=%v want ErrLeaseNotReleased", err)
	}
	if strings.Contains(err.Error(), "completed") || !strings.Contains(err.Error(), `which ended with status "paused"`) {
		t.Fatalf("err=%q should report the run's own status and not call it completed", err)
	}
}

// TestReleaseContextSurvivesCancellationButIsBounded checks the context the
// local release runs under. It must outlive the Ctrl-C that cancelled the
// command, or the lease would be left behind in exactly the case the release is
// for, and it must carry a deadline, or a stalled database would hang the
// process after the interrupt. The test does not prove ExecuteLocal passes this
// context to ReleaseRunLease: no test double sits between them.
func TestReleaseContextSurvivesCancellationButIsBounded(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	before := time.Now()
	ctx, stop := releaseContext(parent)
	defer stop()
	if err := ctx.Err(); err != nil {
		t.Fatalf("release context inherited the cancellation: %v", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("release context has no deadline")
	}
	if remaining := deadline.Sub(before); remaining <= 0 || remaining > releaseTimeout+time.Second {
		t.Fatalf("release deadline is %v away, want at most about %v", remaining, releaseTimeout)
	}
}

// TestExecuteLocalReportsALostLease expires the lease behind the holder's back
// with a controllable clock, then lets renewal notice. The result must wrap
// ErrFenced and fn's context must be cancelled, so a displaced local process
// stops spending model calls on a run it can no longer commit.
func TestExecuteLocalReportsALostLease(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	run := seedRun(ctx, t, s, manifest, "")
	clock := newClock()

	cfg := Config{ID: "local-test", LeaseTTL: 2 * time.Second, RenewEvery: 20 * time.Millisecond, Clock: clock.Clock(), Logger: quietLogger()}
	cancelled := false
	_, err := ExecuteLocal(ctx, s, run.ID, cfg, func(ctx context.Context, fence store.Fence) (store.Run, error) {
		// The lease lapses and another owner takes the run.
		clock.Advance(time.Minute)
		// WithoutCancel: a renewal tick landing between Advance and this
		// acquire cancels ctx, and the takeover must not depend on that race.
		if _, err := s.AcquireRunLease(context.WithoutCancel(ctx), run.ID, "worker-z", time.Minute, clock.Now()); err != nil {
			return store.Run{}, err
		}
		select {
		case <-ctx.Done():
			cancelled = true
		case <-time.After(10 * time.Second):
		}
		return store.Run{}, ctx.Err()
	})
	if !errors.Is(err, store.ErrFenced) {
		t.Fatalf("err=%v want ErrFenced", err)
	}
	if !cancelled {
		t.Fatal("fn context was not cancelled when the lease was lost")
	}
	lease, err := s.RunLease(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Owner != "worker-z" {
		t.Fatalf("displaced holder disturbed the new owner: %+v", lease)
	}
}

func TestLocalOwnerNamesHostAndPID(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	if want := fmt.Sprintf("local-%s-%d", host, os.Getpid()); LocalOwner() != want {
		t.Fatalf("owner=%q want %q", LocalOwner(), want)
	}
}

// TestExecuteLocalDefaultsToTheShortLocalTTL pins that DefaultLocalLeaseTTL is
// applied before withDefaults, which would otherwise substitute the fleet's
// longer TTL. A manual clock makes the expiry exact.
func TestExecuteLocalDefaultsToTheShortLocalTTL(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	run := seedRun(ctx, t, s, manifest, "")
	clock := newClock()
	now := clock.Now()

	_, err := ExecuteLocal(ctx, s, run.ID, Config{Clock: clock.Clock(), Logger: quietLogger()}, func(ctx context.Context, fence store.Fence) (store.Run, error) {
		lease, err := s.RunLease(ctx, run.ID)
		if err != nil {
			return store.Run{}, err
		}
		if want := now.Add(DefaultLocalLeaseTTL); !lease.ExpiresAt.Equal(want) {
			t.Errorf("ExpiresAt=%s want %s (now+%s)", lease.ExpiresAt, want, DefaultLocalLeaseTTL)
		}
		return store.Run{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
