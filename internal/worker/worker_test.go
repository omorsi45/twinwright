package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"twinwright/internal/agent"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/eval"
	"twinwright/internal/pgtest"
	"twinwright/internal/store"
)

// testClock is a manually advanced clock. Lease expiry is the mechanism under
// test here, so driving it by hand is what makes these tests deterministic
// rather than timing-dependent.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *testClock {
	return &testClock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
}
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
func (c *testClock) Clock() store.Clock { return func() time.Time { return c.Now() } }

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func billingManifest(t *testing.T) compiler.Manifest {
	t.Helper()
	root := filepath.Join("..", "..", "examples", "billing")
	spec, err := os.ReadFile(filepath.Join(root, "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := os.ReadFile(filepath.Join(root, "bindings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := compiler.Compile(spec, bindings)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

// openStore uses PostgreSQL when a DSN is configured and SQLite otherwise, so
// the distributed semantics are exercised on both backends. SQLite serialises
// writers, which still proves the fencing logic; PostgreSQL additionally proves
// it under genuinely concurrent connections.
func openStore(ctx context.Context, t *testing.T) *store.Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "worker.db")
	if pgtest.Available() {
		dsn = pgtest.SchemaDSN(t)
	}
	s, err := store.OpenDSN(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedRun(ctx context.Context, t *testing.T, s *store.Store, manifest compiler.Manifest, fault string) store.Run {
	t.Helper()
	world, err := s.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "test", "test-model", "task", fault)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// realExecutor runs the scripted duplicate-charge agent under the lease's fence.
func realExecutor(s *store.Store, manifest compiler.Manifest, clock store.Clock) ExecuteFunc {
	return func(ctx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		fence := lease.Fence()
		runner := agent.Runner{
			Store:    s,
			Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &fence, Clock: clock},
			Manifest: manifest,
			Provider: agent.ScriptedProvider{},
			Fence:    &fence,
			Clock:    clock,
		}
		return runner.Execute(ctx, lease.RunID, entry.MaxSteps)
	}
}

func TestWorkerClaimsAndCompletesAQueuedRun(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	w := New(s, realExecutor(s, manifest, clock.Clock()), Config{ID: "worker-a", LeaseTTL: time.Minute, Clock: clock.Clock(), Logger: quietLogger()})
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if outcome.Disposition != Finished {
		t.Fatalf("outcome=%+v", outcome)
	}
	if outcome.RunStatus != "completed" {
		t.Fatalf("run status=%s", outcome.RunStatus)
	}
	entry, err := s.QueueEntryFor(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.State != store.QueueFinished {
		t.Fatalf("queue state=%s", entry.State)
	}
	// The lease must be given up so the run is not stuck owned forever.
	lease, err := s.RunLease(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Owner != "" {
		t.Fatalf("finished run is still owned by %q", lease.Owner)
	}
	// And the world must be correct, not merely marked done.
	world, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	report, err := eval.DuplicateCharge(ctx, s, world.WorldID)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Fatalf("evaluation=%+v", report)
	}
	// A second poll must find nothing: a finished run is not re-delivered.
	if _, err = w.ClaimAndRun(ctx); !errors.Is(err, store.ErrNoWork) {
		t.Fatalf("second claim err=%v want ErrNoWork", err)
	}
}

// TestCrashedWorkerIsTakenOverAndRunCompletesOnce is the central distributed
// scenario: worker A claims, stalls without releasing (what a killed process
// leaves behind), the lease expires, worker B takes over with a higher fence and
// finishes the run, and worker A's late commit is refused.
func TestCrashedWorkerIsTakenOverAndRunCompletesOnce(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}

	// Worker A claims the run and gets one tool call in, then dies. It never
	// releases the lease and never updates the queue.
	var stalledLease store.RunLease
	crashing := func(ctx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		stalledLease = lease
		fence := lease.Fence()
		d := &dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &fence, Clock: clock.Clock()}
		if _, err := d.Invoke(ctx, lease.RunID, "call-a1", "getCharge", map[string]any{"id": "CH-1002"}); err != nil {
			return store.Run{}, err
		}
		return store.Run{}, errors.New("worker-a process died")
	}
	a := New(s, crashing, Config{ID: "worker-a", LeaseTTL: time.Minute, MaxAttempts: 5, Clock: clock.Clock(), Logger: quietLogger()})
	if err := a.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcomeA, err := a.ClaimAndRun(ctx)
	if err != nil {
		t.Fatalf("worker-a claim: %v", err)
	}
	if outcomeA.Disposition != Requeued {
		t.Fatalf("worker-a outcome=%+v want requeued", outcomeA)
	}

	// Simulate the harder case too: put the entry back into the leased state
	// with A still nominally owning it, which is what a hard kill produces.
	if _, err = s.DB.ExecContext(ctx, "UPDATE work_queue SET state=?, available_at=? WHERE run_id=?",
		store.QueueLeased, clock.Now().UTC().Format(time.RFC3339Nano), run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.ExecContext(ctx, "UPDATE run_leases SET owner=?, expires_at=? WHERE run_id=?",
		"worker-a", clock.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano), run.ID); err != nil {
		t.Fatal(err)
	}

	// Before expiry, worker B must not be able to steal the run.
	b := New(s, realExecutor(s, manifest, clock.Clock()), Config{ID: "worker-b", LeaseTTL: time.Minute, Clock: clock.Clock(), Logger: quietLogger()})
	if err = b.Register(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = b.ClaimAndRun(ctx); !errors.Is(err, store.ErrNoWork) {
		t.Fatalf("worker-b claimed a live lease: err=%v", err)
	}

	// After expiry it takes over.
	clock.Advance(2 * time.Minute)
	outcomeB, err := b.ClaimAndRun(ctx)
	if err != nil {
		t.Fatalf("worker-b claim: %v", err)
	}
	if outcomeB.Disposition != Finished {
		t.Fatalf("worker-b outcome=%+v", outcomeB)
	}
	if outcomeB.Fence <= stalledLease.Token {
		t.Fatalf("takeover fence %d did not exceed the dead worker's %d", outcomeB.Fence, stalledLease.Token)
	}
	if !outcomeB.Takeover {
		t.Fatal("takeover was not reported as such")
	}

	// Worker A wakes up and tries to finish the work it thought it owned.
	fence := stalledLease.Fence()
	stale := &dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &fence, Clock: clock.Clock()}
	_, err = stale.Invoke(ctx, run.ID, "call-a-late", "createRefund", map[string]any{"charge_id": "CH-1002", "amount_cents": 500, "reason": "late"})
	if !errors.Is(err, store.ErrFenced) {
		t.Fatalf("stale worker commit err=%v want ErrFenced", err)
	}

	// Exactly one refund, and the world passes evaluation.
	current, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var refunds int
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", current.WorldID).Scan(&refunds); err != nil {
		t.Fatal(err)
	}
	if refunds != 1 {
		t.Fatalf("refunds=%d want exactly 1 after takeover", refunds)
	}
	report, err := eval.DuplicateCharge(ctx, s, current.WorldID)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Fatalf("evaluation after takeover=%+v", report)
	}
}

// TestDuplicateDeliveryDoesNotDuplicateCommittedEffects states the delivery
// contract directly: the same run delivered twice, with the same call IDs,
// commits its side effects once.
func TestDuplicateDeliveryDoesNotDuplicateCommittedEffects(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	w := New(s, realExecutor(s, manifest, clock.Clock()), Config{ID: "worker-a", LeaseTTL: time.Minute, Clock: clock.Clock(), Logger: quietLogger()})
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ClaimAndRun(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var first int
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", current.WorldID).Scan(&first); err != nil {
		t.Fatal(err)
	}

	// Re-enqueue the very same run: at-least-once delivery in its purest form.
	clock.Advance(time.Minute)
	if err = s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	outcome, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if outcome.Disposition != Finished {
		t.Fatalf("redelivery outcome=%+v", outcome)
	}
	var second int
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", current.WorldID).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("redelivery changed committed effects: %d then %d", first, second)
	}
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	mutations := 0
	for _, event := range events {
		if event.Type == "state.mutation" {
			mutations++
		}
	}
	if mutations != 1 {
		t.Fatalf("state mutations=%d want 1 across two deliveries", mutations)
	}
}

// TestRenewalLossCancelsExecutionMidFlight proves the worker stops working the
// moment it loses the lease, rather than finishing a run it cannot commit.
func TestRenewalLossCancelsExecutionMidFlight(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	stolen := make(chan struct{})
	blocking := func(execCtx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		// Another worker takes the run over while this one is mid-execution.
		clock.Advance(2 * time.Minute)
		if _, err := s.AcquireRunLease(ctx, lease.RunID, "worker-b", time.Minute, clock.Now()); err != nil {
			return store.Run{}, err
		}
		close(stolen)
		select {
		case <-execCtx.Done():
			return store.Run{}, execCtx.Err()
		case <-time.After(10 * time.Second):
			return store.Run{}, errors.New("execution was never cancelled after the lease was lost")
		}
	}
	w := New(s, blocking, Config{ID: "worker-a", LeaseTTL: time.Minute, RenewEvery: 50 * time.Millisecond, Clock: clock.Clock(), Logger: quietLogger()})
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	<-stolen
	if outcome.Disposition != Fenced {
		t.Fatalf("outcome=%+v want fenced", outcome)
	}
	// The queue entry must still belong to worker B's attempt, not be rewritten
	// by the worker that lost it.
	entry, err := s.QueueEntryFor(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.State != store.QueueLeased {
		t.Fatalf("a fenced worker rewrote the queue entry to %q", entry.State)
	}
	history, err := s.Ownership(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rejected := false
	for _, event := range history {
		if event.Kind == store.OwnershipFenced {
			rejected = true
		}
	}
	if !rejected {
		t.Fatal("losing the lease was not recorded in the ownership log")
	}
}

// TestIndependentRunsProgressInParallel checks that several workers make
// progress on distinct runs at the same time rather than serialising on one.
func TestIndependentRunsProgressInParallel(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	const runs = 4
	ids := make([]string, 0, runs)
	for i := 0; i < runs; i++ {
		run := seedRun(ctx, t, s, manifest, "")
		if err := s.Enqueue(ctx, run.ID, 10, clock.Now().Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID)
	}
	var mu sync.Mutex
	claimed := map[string]string{}
	var wg sync.WaitGroup
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := fmt.Sprintf("worker-%d", n)
			w := New(s, realExecutor(s, manifest, clock.Clock()), Config{ID: id, LeaseTTL: time.Minute, Clock: clock.Clock(), Logger: quietLogger()})
			if err := w.Register(ctx); err != nil {
				t.Errorf("register %s: %v", id, err)
				return
			}
			for {
				outcome, err := w.ClaimAndRun(ctx)
				if errors.Is(err, store.ErrNoWork) {
					return
				}
				if err != nil {
					t.Errorf("%s claim: %v", id, err)
					return
				}
				if outcome.Disposition != Finished {
					t.Errorf("%s outcome=%+v", id, outcome)
					return
				}
				mu.Lock()
				claimed[outcome.RunID] = id
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if len(claimed) != runs {
		t.Fatalf("claimed %d of %d runs: %v", len(claimed), runs, claimed)
	}
	for _, id := range ids {
		entry, err := s.QueueEntryFor(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if entry.State != store.QueueFinished {
			t.Fatalf("run %s ended in state %q", id, entry.State)
		}
		if entry.Attempts != 1 {
			t.Fatalf("run %s was attempted %d times; independent runs must not be re-delivered", id, entry.Attempts)
		}
	}
}

// TestConcurrentWorkersDoNotDoubleClaimOneRun points many workers at a single
// queued run at the same instant. Exactly one must win.
func TestConcurrentWorkersDoNotDoubleClaimOneRun(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	const contenders = 6
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var winners []string
	var fences []int64
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			owner := fmt.Sprintf("worker-%d", n)
			<-start
			lease, _, err := s.ClaimRun(ctx, owner, time.Minute, clock.Now())
			if errors.Is(err, store.ErrNoWork) {
				return
			}
			if err != nil {
				// A serialisation failure is a legitimate loss, not a
				// double claim; only a successful claim counts as winning.
				return
			}
			mu.Lock()
			winners = append(winners, owner)
			fences = append(fences, lease.Token)
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()
	if len(winners) != 1 {
		t.Fatalf("%d workers claimed the same run: %v (fences %v)", len(winners), winners, fences)
	}
}
