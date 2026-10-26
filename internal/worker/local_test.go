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

// TestExecuteLocalRenewsItsLease uses the real clock: the lease would lapse at
// 2s, so a guard that still passes at 3s proves the renewal loop extended it.
func TestExecuteLocalRenewsItsLease(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	run := seedRun(ctx, t, s, manifest, "")

	cfg := Config{LeaseTTL: 2 * time.Second, RenewEvery: time.Second, Logger: quietLogger()}
	_, err := ExecuteLocal(ctx, s, run.ID, cfg, func(ctx context.Context, fence store.Fence) (store.Run, error) {
		time.Sleep(3 * time.Second)
		if err := s.GuardFence(ctx, fence, time.Now().UTC()); err != nil {
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
		return store.Run{}, nil
	})
	if !errors.Is(err, ErrLeaseNotReleased) {
		t.Fatalf("err=%v want ErrLeaseNotReleased", err)
	}
	if !strings.Contains(err.Error(), "database is closed") || strings.Contains(err.Error(), "failed") {
		t.Fatalf("err=%q should carry the cause and not claim the run failed", err)
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
