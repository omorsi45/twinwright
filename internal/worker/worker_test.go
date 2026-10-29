package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"twinwright/internal/agent"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/eval"
	"twinwright/internal/pgtest"
	"twinwright/internal/store"
	"twinwright/internal/trace"
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
		store.QueueLeased, store.CoordinationTime(clock.Now()), run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.ExecContext(ctx, "UPDATE run_leases SET owner=?, expires_at=? WHERE run_id=?",
		"worker-a", store.CoordinationTime(clock.Now().Add(time.Minute)), run.ID); err != nil {
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

// TestDuplicateDeliveryDoesNotDuplicateCommittedEffects shows that delivering a
// finished run a second time commits nothing new. It does not exercise the
// call-ID cache: the second delivery returns at the run's completed status
// (internal/agent/runner.go), before any call is dispatched, so no call ID is
// presented twice. TestCommittedCallIsAnsweredFromTheCacheUnderTheNewOwner is the
// test of that.
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
		// Enqueue times are distinct but already past, so every run is claimable
		// at clock.Now(). A future enqueue time would make later runs correctly
		// unavailable now that availability orders by time rather than by text.
		if err := s.Enqueue(ctx, run.ID, 10, clock.Now().Add(time.Duration(i-runs)*time.Millisecond)); err != nil {
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

// recordingLogger keeps every record so a test can assert what an operator
// would have seen.
func recordingLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// TestClaimFinishedCarriesTheTraceID lets an operator go from the worker's log
// line to the collector's trace without recomputing the ID.
func TestClaimFinishedCarriesTheTraceID(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	w := New(s, realExecutor(s, manifest, clock.Clock()), Config{ID: "worker-a", LeaseTTL: time.Minute, Clock: clock.Clock(), Logger: recordingLogger(&logs)})
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := trace.TraceIDFor(run.ID)
	if outcome.TraceID != want {
		t.Fatalf("outcome trace id=%q want %q", outcome.TraceID, want)
	}
	if outcome.TraceExported {
		t.Fatal("a worker with no ExportTrace reports an export")
	}
	if !strings.Contains(logs.String(), "msg=\"claim finished\"") || !strings.Contains(logs.String(), "trace_id="+want) {
		t.Fatalf("the claim finished line does not carry trace_id=%s:\n%s", want, logs.String())
	}
}

func TestWorkerExportsACompletedRunsTraceThroughTheHook(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	var exported []string
	var deadline time.Time
	var hasDeadline bool
	var logs bytes.Buffer
	w := New(s, realExecutor(s, manifest, clock.Clock()), Config{
		ID: "worker-a", LeaseTTL: time.Minute, Clock: clock.Clock(), Logger: recordingLogger(&logs),
		ExportTrace: func(exportCtx context.Context, runID string) error {
			exported = append(exported, runID)
			deadline, hasDeadline = exportCtx.Deadline()
			return nil
		},
	})
	var observed []Outcome
	w.Observer = func(o Outcome) { observed = append(observed, o) }
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(exported) != 1 || exported[0] != run.ID {
		t.Fatalf("exported=%v want [%s]", exported, run.ID)
	}
	if !hasDeadline {
		t.Fatal("the export context has no deadline, so a stuck collector could hold the worker")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > trace.DefaultExportTimeout {
		t.Fatalf("export deadline is %v away, want within (0, %v]", remaining, trace.DefaultExportTimeout)
	}
	if outcome.Disposition != Finished || !outcome.TraceExported || outcome.TraceID != trace.TraceIDFor(run.ID) {
		t.Fatalf("outcome=%+v", outcome)
	}
	// Observers (the metrics endpoint, tests) must see the finished outcome,
	// export result included, not a copy taken before the export ran.
	if len(observed) != 1 || !observed[0].TraceExported {
		t.Fatalf("observer saw %+v", observed)
	}
	if strings.Contains(logs.String(), "trace export failed") {
		t.Fatalf("a successful export logged a failure:\n%s", logs.String())
	}
}

// TestWorkerExportFailureKeepsTheDisposition: the export is best effort. The
// run is done and recorded; a collector outage must not requeue or fail it.
func TestWorkerExportFailureKeepsTheDisposition(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	w := New(s, realExecutor(s, manifest, clock.Clock()), Config{
		ID: "worker-a", LeaseTTL: time.Minute, Clock: clock.Clock(), Logger: recordingLogger(&logs),
		ExportTrace: func(context.Context, string) error { return errors.New("collector down") },
	})
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatalf("a failed export surfaced as an error: %v", err)
	}
	if outcome.Disposition != Finished || outcome.RunStatus != "completed" || outcome.Err != "" {
		t.Fatalf("outcome=%+v", outcome)
	}
	if outcome.TraceExported {
		t.Fatal("outcome reports an export that failed")
	}
	entry, err := s.QueueEntryFor(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.State != store.QueueFinished {
		t.Fatalf("queue state=%q want finished", entry.State)
	}
	warning := ""
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "trace export failed") {
			warning = line
		}
	}
	if warning == "" {
		t.Fatalf("no warning was logged:\n%s", logs.String())
	}
	for _, want := range []string{"level=WARN", "worker=worker-a", "run=" + run.ID, "trace_id=" + trace.TraceIDFor(run.ID), "collector down"} {
		if !strings.Contains(warning, want) {
			t.Fatalf("warning %q lacks %q", warning, want)
		}
	}
}

// TestWorkerExportsOnlyWhenTheClaimFinishedACompletedRun covers the claims that
// must not export: a run paused at its step budget is not finished work, and a
// failed attempt has no complete trace to send.
func TestWorkerExportsOnlyWhenTheClaimFinishedACompletedRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		steps   int
		execute func(*store.Store, compiler.Manifest, store.Clock) ExecuteFunc
		want    Disposition
		status  string
	}{
		{"paused at the step budget", 1, func(s *store.Store, m compiler.Manifest, c store.Clock) ExecuteFunc { return realExecutor(s, m, c) }, Finished, "paused"},
		{"failed attempt is requeued", 10, func(*store.Store, compiler.Manifest, store.Clock) ExecuteFunc {
			return func(context.Context, store.RunLease, store.QueueEntry) (store.Run, error) {
				return store.Run{}, errors.New("provider unavailable")
			}
		}, Requeued, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			manifest := billingManifest(t)
			s := openStore(ctx, t)
			clock := newClock()
			run := seedRun(ctx, t, s, manifest, "")
			if err := s.Enqueue(ctx, run.ID, tc.steps, clock.Now()); err != nil {
				t.Fatal(err)
			}
			calls := 0
			w := New(s, tc.execute(s, manifest, clock.Clock()), Config{
				ID: "worker-a", LeaseTTL: time.Minute, MaxAttempts: 5, Clock: clock.Clock(), Logger: quietLogger(),
				ExportTrace: func(context.Context, string) error { calls++; return nil },
			})
			if err := w.Register(ctx); err != nil {
				t.Fatal(err)
			}
			outcome, err := w.ClaimAndRun(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Disposition != tc.want || outcome.RunStatus != tc.status {
				t.Fatalf("outcome=%+v want %s/%q", outcome, tc.want, tc.status)
			}
			if calls != 0 || outcome.TraceExported {
				t.Fatalf("exported %d times for a claim that did not complete the run: %+v", calls, outcome)
			}
			if outcome.TraceID != trace.TraceIDFor(run.ID) {
				t.Fatalf("trace id=%q want %q: every outcome with a run carries it", outcome.TraceID, trace.TraceIDFor(run.ID))
			}
		})
	}
}

// TestWorkerExportsARunSettledPastTheDeliveryLimit: the delivery-limit path
// finishes a run whose work completed before a crash, and that run is still a
// completed run whose trace has not necessarily been sent.
func TestWorkerExportsARunSettledPastTheDeliveryLimit(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	const maxAttempts = 3
	for i := 1; i <= maxAttempts; i++ {
		lease := killDelivery(ctx, t, s, clock, i)
		if i == maxAttempts {
			completeUnderFence(ctx, t, s, manifest, clock, lease)
		}
		clock.Advance(2 * time.Minute)
	}
	var exported []string
	neverRuns := func(context.Context, store.RunLease, store.QueueEntry) (store.Run, error) {
		t.Fatal("a completed run was executed again")
		return store.Run{}, nil
	}
	b := New(s, neverRuns, Config{
		ID: "worker-b", LeaseTTL: time.Minute, MaxAttempts: maxAttempts, Clock: clock.Clock(), Logger: quietLogger(),
		ExportTrace: func(_ context.Context, runID string) error { exported = append(exported, runID); return nil },
	})
	if err := b.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := b.ClaimAndRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != Finished || !outcome.TraceExported || len(exported) != 1 || exported[0] != run.ID {
		t.Fatalf("outcome=%+v exported=%v", outcome, exported)
	}
}

// TestRenewalsAreCounted holds a run across several renewals and checks that
// each successful one is reported through OnRenew with the lease the store
// returned. Renewal is the only liveness signal a long run gives, and before
// this hook the renewals counter could never move.
func TestRenewalsAreCounted(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var renewed []store.RunLease
	enough := make(chan struct{})
	record := func(lease store.RunLease) {
		mu.Lock()
		defer mu.Unlock()
		renewed = append(renewed, lease)
		switch len(renewed) {
		case 1:
			// Move the manual clock so the next renewal visibly extends the lease.
			clock.Advance(10 * time.Second)
		case 2:
			close(enough)
		}
	}
	execute := realExecutor(s, manifest, clock.Clock())
	hold := func(execCtx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		select {
		case <-enough:
		case <-execCtx.Done():
			return store.Run{}, execCtx.Err()
		case <-time.After(10 * time.Second):
			return store.Run{}, errors.New("the lease was not renewed twice while the run was held")
		}
		return execute(execCtx, lease, entry)
	}
	w := New(s, hold, Config{
		ID: "worker-a", LeaseTTL: time.Minute, RenewEvery: 10 * time.Millisecond,
		Clock: clock.Clock(), Logger: quietLogger(), OnRenew: record,
	})
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != Finished {
		t.Fatalf("outcome=%+v", outcome)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(renewed) < 2 {
		t.Fatalf("renewals reported=%d want at least 2", len(renewed))
	}
	for _, lease := range renewed {
		if lease.RunID != run.ID || lease.Owner != "worker-a" || lease.Token != outcome.Fence {
			t.Fatalf("OnRenew got %+v, want the claim's own lease (run %s, fence %d)", lease, run.ID, outcome.Fence)
		}
	}
	if !renewed[1].ExpiresAt.After(renewed[0].ExpiresAt) {
		t.Fatalf("OnRenew did not get the renewed lease: expiry %v then %v", renewed[0].ExpiresAt, renewed[1].ExpiresAt)
	}
}

// TestFailedRenewalIsNotCounted: a renewal the store refuses means the run
// moved on, and counting it would report liveness the worker does not have.
func TestFailedRenewalIsNotCounted(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := 0
	stolen := func(execCtx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		// Another worker takes the run before the first renewal is due.
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
	w := New(s, stolen, Config{
		ID: "worker-a", LeaseTTL: time.Minute, RenewEvery: 250 * time.Millisecond, Clock: clock.Clock(), Logger: quietLogger(),
		OnRenew: func(store.RunLease) { mu.Lock(); calls++; mu.Unlock() },
	})
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
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("OnRenew was called %d times for renewals the store refused", calls)
	}
}
