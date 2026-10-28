package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"twinwright/internal/agent"
	"twinwright/internal/compiler"
	"twinwright/internal/crashpoint"
	"twinwright/internal/dispatch"
	"twinwright/internal/replay"
	"twinwright/internal/store"
)

// TestRequeueAfterAnErrorIsNotReportedAsATakeover separates two things the
// attempt counter used to conflate. The first worker returned an error and
// released its lease, so the second claim displaced nobody: it is a retry.
func TestRequeueAfterAnErrorIsNotReportedAsATakeover(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	real := realExecutor(s, manifest, clock.Clock())
	calls := 0
	flaky := func(ctx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		calls++
		if calls == 1 {
			return store.Run{}, errors.New("transient failure")
		}
		return real(ctx, lease, entry)
	}
	w := New(s, flaky, Config{ID: "worker-a", LeaseTTL: time.Minute, MaxAttempts: 3, Backoff: time.Second, Clock: clock.Clock(), Logger: quietLogger()})
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if first.Disposition != Requeued {
		t.Fatalf("first outcome=%+v want requeued", first)
	}
	clock.Advance(2 * time.Second)
	second, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second.Attempt != 2 {
		t.Fatalf("second attempt=%d want 2", second.Attempt)
	}
	if second.Takeover {
		t.Fatalf("a requeue after an error was reported as a takeover: %+v", second)
	}
}

// TestExpiredLeaseClaimIsReportedAsATakeover is the other side: a claim that
// displaces a lease left behind by a dead worker is a takeover.
func TestExpiredLeaseClaimIsReportedAsATakeover(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, clock.Now()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Minute)
	b := New(s, realExecutor(s, manifest, clock.Clock()), Config{ID: "worker-b", LeaseTTL: time.Minute, Clock: clock.Clock(), Logger: quietLogger()})
	if err := b.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := b.ClaimAndRun(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !outcome.Takeover {
		t.Fatalf("a claim that displaced an expired lease was not reported as a takeover: %+v", outcome)
	}
}

// killDelivery claims the run as a worker that then dies without reporting:
// the lease is left to expire, which is all a crash leaves behind.
func killDelivery(ctx context.Context, t *testing.T, s *store.Store, clock *testClock, delivery int) store.RunLease {
	t.Helper()
	lease, _, err := s.ClaimRun(ctx, fmt.Sprintf("doomed-%d", delivery), time.Minute, clock.Now())
	if err != nil {
		t.Fatalf("doomed claim %d: %v", delivery, err)
	}
	return lease
}

func TestRunThatKillsEveryDeliveryIsFailedAtTheDeliveryLimit(t *testing.T) {
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
		killDelivery(ctx, t, s, clock, i)
		clock.Advance(2 * time.Minute)
	}
	neverRuns := func(context.Context, store.RunLease, store.QueueEntry) (store.Run, error) {
		t.Fatal("a run past its delivery limit was executed")
		return store.Run{}, nil
	}
	b := New(s, neverRuns, Config{ID: "worker-b", LeaseTTL: time.Minute, MaxAttempts: maxAttempts, Clock: clock.Clock(), Logger: quietLogger()})
	if err := b.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := b.ClaimAndRun(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if outcome.Disposition != Failed || outcome.Attempt != maxAttempts+1 {
		t.Fatalf("outcome=%+v want failed at attempt %d", outcome, maxAttempts+1)
	}
	if outcome.Err == "" {
		t.Fatal("a failed outcome carries no reason")
	}

	current, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "failed" {
		t.Fatalf("run status=%q want failed", current.Status)
	}
	last, err := s.LastEvent(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if last.Type != "error" {
		t.Fatalf("last event type=%q want error", last.Type)
	}
	var payload map[string]any
	if err = json.Unmarshal(last.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["kind"] != "delivery_limit" || payload["deliveries"] != float64(maxAttempts+1) || payload["max_attempts"] != float64(maxAttempts) {
		t.Fatalf("payload=%v", payload)
	}
	// The ledger is the agent trajectory and never names a worker, a fence or
	// a lease, so the payload may hold nothing beyond these three keys.
	for key := range payload {
		if key != "kind" && key != "deliveries" && key != "max_attempts" {
			t.Fatalf("payload carries unexpected key %q: %v", key, payload)
		}
	}

	entry, err := s.QueueEntryFor(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.State != store.QueueFailed {
		t.Fatalf("queue state=%q want failed", entry.State)
	}
	if _, _, err = s.ClaimRun(ctx, "worker-c", time.Minute, clock.Now().Add(time.Hour)); !errors.Is(err, store.ErrNoWork) {
		t.Fatalf("claim of a failed run err=%v want ErrNoWork", err)
	}
}

// TestCompletedRunPastTheDeliveryLimitIsFinishedNotFailed covers a worker that
// dies after the agent completed but before it marked the queue entry finished.
// The run did its work, so failing it for the delivery count would misreport it.
func TestCompletedRunPastTheDeliveryLimitIsFinishedNotFailed(t *testing.T) {
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
	neverRuns := func(context.Context, store.RunLease, store.QueueEntry) (store.Run, error) {
		t.Fatal("a completed run was executed again")
		return store.Run{}, nil
	}
	b := New(s, neverRuns, Config{ID: "worker-b", LeaseTTL: time.Minute, MaxAttempts: maxAttempts, Clock: clock.Clock(), Logger: quietLogger()})
	if err := b.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := b.ClaimAndRun(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if outcome.Disposition != Finished || outcome.Attempt != maxAttempts+1 {
		t.Fatalf("outcome=%+v want finished at attempt %d", outcome, maxAttempts+1)
	}
	current, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "completed" {
		t.Fatalf("run status=%q want completed", current.Status)
	}
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != "error" {
			continue
		}
		var payload map[string]any
		if err = json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["kind"] == "delivery_limit" {
			t.Fatalf("a completed run was recorded as hitting the delivery limit: %v", payload)
		}
	}
	entry, err := s.QueueEntryFor(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.State != store.QueueFinished {
		t.Fatalf("queue state=%q want finished", entry.State)
	}
}

// TestReenqueuedFailedRunGetsAFreshDeliveryBudget covers the operator retry that
// Enqueue exists for. A run failed at the delivery limit carries attempts past
// MaxAttempts, so without a reset the next claim would refuse it again before it
// executed. An explicit retry starts a new delivery budget; the earlier
// ownership history stays in run_ownership_log.
func TestReenqueuedFailedRunGetsAFreshDeliveryBudget(t *testing.T) {
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
		killDelivery(ctx, t, s, clock, i)
		clock.Advance(2 * time.Minute)
	}
	neverRuns := func(context.Context, store.RunLease, store.QueueEntry) (store.Run, error) {
		t.Fatal("a run past its delivery limit was executed")
		return store.Run{}, nil
	}
	limiter := New(s, neverRuns, Config{ID: "worker-limit", LeaseTTL: time.Minute, MaxAttempts: maxAttempts, Clock: clock.Clock(), Logger: quietLogger()})
	if err := limiter.Register(ctx); err != nil {
		t.Fatal(err)
	}
	failed, err := limiter.ClaimAndRun(ctx)
	if err != nil {
		t.Fatalf("claim at the limit: %v", err)
	}
	if failed.Disposition != Failed {
		t.Fatalf("outcome=%+v want failed at the delivery limit", failed)
	}

	if err = s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	retry := New(s, realExecutor(s, manifest, clock.Clock()), Config{ID: "worker-retry", LeaseTTL: time.Minute, MaxAttempts: maxAttempts, Clock: clock.Clock(), Logger: quietLogger()})
	if err = retry.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := retry.ClaimAndRun(ctx)
	if err != nil {
		t.Fatalf("claim after re-enqueue: %v", err)
	}
	if outcome.Disposition != Finished || outcome.Attempt != 1 {
		t.Fatalf("outcome=%+v want finished at attempt 1", outcome)
	}
	current, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "completed" {
		t.Fatalf("run status=%q want completed", current.Status)
	}
}

// completeUnderFence runs the scripted agent to completion under lease's fence
// and stops there, without FinishRun: the worker died after completion.
func completeUnderFence(ctx context.Context, t *testing.T, s *store.Store, manifest compiler.Manifest, clock *testClock, lease store.RunLease) {
	t.Helper()
	fence := lease.Fence()
	runner := agent.Runner{
		Store:    s,
		Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &fence, Clock: clock.Clock()},
		Manifest: manifest,
		Provider: agent.ScriptedProvider{},
		Fence:    &fence,
		Clock:    clock.Clock(),
	}
	run, err := runner.Execute(ctx, lease.RunID, 10)
	if err != nil {
		t.Fatalf("execute under doomed lease: %v", err)
	}
	if run.Status != "completed" {
		t.Fatalf("doomed delivery ended as %q want completed", run.Status)
	}
}

// TestTheLastAllowedDeliveryStillExecutes pins the boundary of the delivery
// bound. MaxAttempts counts the deliveries a run may have, so the delivery that
// reaches MaxAttempts is the last one that executes, and only the next one is
// settled without running. Two doomed claims leave the third as that last
// allowed delivery; a bound written as `Attempts >= MaxAttempts` would refuse
// it, fail a run that still had a delivery left, and pass every other test here.
func TestTheLastAllowedDeliveryStillExecutes(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	const maxAttempts = 3
	for i := 1; i < maxAttempts; i++ {
		killDelivery(ctx, t, s, clock, i)
		clock.Advance(2 * time.Minute)
	}
	w := New(s, realExecutor(s, manifest, clock.Clock()), Config{ID: "worker-last", LeaseTTL: time.Minute, MaxAttempts: maxAttempts, Clock: clock.Clock(), Logger: quietLogger()})
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if outcome.Attempt != maxAttempts || outcome.Disposition != Finished {
		t.Fatalf("outcome=%+v want finished at attempt %d", outcome, maxAttempts)
	}
	current, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "completed" {
		t.Fatalf("run status=%q want completed", current.Status)
	}
}

// crashAfterModelRequest is the crash hook for a delivery that dies right after
// it recorded a model request and before it asked the model for an answer.
func crashAfterModelRequest(_ context.Context, site crashpoint.Site) error {
	if site.Point == crashpoint.AfterModelRequest {
		return errors.New("worker process died after the model request")
	}
	return nil
}

// TestRetryAfterTheDeliveryLimitCompletesButDoesNotReplay documents a limit of
// the delivery bound; it does not endorse the outcome. A run that is failed at
// the delivery limit and then retried with Enqueue can complete, but its ledger
// keeps the delivery_limit error and the model request that was never answered
// before it, and the retry appends a new request instead of reusing that one.
// Replay refuses an error of that kind, so the completed run cannot be verified.
//
// Changing replay or the runner to accept this is milestone 3 work with its own
// decision record; until then this test pins what actually happens so the
// documented limit (ADR 0029, Remaining limits) cannot drift unnoticed. If a
// later change makes the run verifiable, this test should fail and be rewritten
// together with that ADR.
func TestRetryAfterTheDeliveryLimitCompletesButDoesNotReplay(t *testing.T) {
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
		lease, _, err := s.ClaimRun(ctx, fmt.Sprintf("doomed-%d", i), time.Minute, clock.Now())
		if err != nil {
			t.Fatalf("doomed claim %d: %v", i, err)
		}
		fence := lease.Fence()
		runner := agent.Runner{
			Store:    s,
			Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &fence, Clock: clock.Clock()},
			Manifest: manifest,
			Provider: agent.ScriptedProvider{},
			Fence:    &fence,
			Clock:    clock.Clock(),
			Crash:    crashAfterModelRequest,
		}
		if _, err = runner.Execute(ctx, run.ID, 10); !errors.Is(err, crashpoint.ErrCrashed) {
			t.Fatalf("doomed delivery %d err=%v want a simulated crash", i, err)
		}
		clock.Advance(2 * time.Minute)
	}

	neverRuns := func(context.Context, store.RunLease, store.QueueEntry) (store.Run, error) {
		t.Fatal("a run past its delivery limit was executed")
		return store.Run{}, nil
	}
	limiter := New(s, neverRuns, Config{ID: "worker-limit", LeaseTTL: time.Minute, MaxAttempts: maxAttempts, Clock: clock.Clock(), Logger: quietLogger()})
	if err := limiter.Register(ctx); err != nil {
		t.Fatal(err)
	}
	failed, err := limiter.ClaimAndRun(ctx)
	if err != nil || failed.Disposition != Failed {
		t.Fatalf("claim at the limit: outcome=%+v err=%v want failed", failed, err)
	}

	if err = s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	retry := New(s, realExecutor(s, manifest, clock.Clock()), Config{ID: "worker-retry", LeaseTTL: time.Minute, MaxAttempts: maxAttempts, Clock: clock.Clock(), Logger: quietLogger()})
	if err = retry.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := retry.ClaimAndRun(ctx)
	if err != nil || outcome.Disposition != Finished {
		t.Fatalf("retry: outcome=%+v err=%v want finished", outcome, err)
	}
	current, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "completed" {
		t.Fatalf("run status=%q want completed", current.Status)
	}

	report, err := replay.Verify(ctx, s, run.ID, manifest)
	if err == nil {
		t.Fatalf("replay verified a run retried after the delivery limit: %+v", report)
	}
	if !strings.Contains(err.Error(), "delivery_limit") {
		t.Fatalf("replay err=%v want it to name the delivery_limit error", err)
	}
}
