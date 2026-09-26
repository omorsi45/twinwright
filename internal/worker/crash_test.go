package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"twinwright/internal/agent"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/eval"
	"twinwright/internal/replay"
	"twinwright/internal/store"
)

// crashAtProvider kills the run at a chosen model turn.
//
// Cancelling the context on the Nth provider call reproduces a process death at
// a precise durable boundary: the runner has already persisted the model request
// for turn N, and everything from turns 1..N-1 - model responses, tool
// requests, tool responses, state mutations - is committed. Because the
// cancellation also prevents the runner from recording the provider failure,
// the ledger is left exactly as a `kill -9` would leave it, ending in an
// unanswered model request.
type crashAtProvider struct {
	inner  agent.Provider
	calls  *int
	at     int
	cancel context.CancelFunc
}

func (p crashAtProvider) Next(ctx context.Context, task string, history []agent.Message, ops []compiler.Operation) (agent.Message, error) {
	*p.calls++
	if *p.calls == p.at {
		p.cancel()
		return agent.Message{}, errors.New("worker process died mid-turn")
	}
	return p.inner.Next(ctx, task, history, ops)
}

// TestRecoveryFromCrashAtEachModelTurn walks the crash point through every model
// turn of a real scripted run.
//
// For each boundary it checks the four things that make recovery meaningful
// rather than merely live: another worker finishes the run, the world is
// correct, the refund committed exactly once despite the run being delivered
// twice, and the recovered ledger still verifies under replay. The replay check
// matters most - a crash that leaves an unauditable run has lost the property
// Twinwright exists to provide.
func TestRecoveryFromCrashAtEachModelTurn(t *testing.T) {
	for turn := 1; turn <= 4; turn++ {
		t.Run(fmt.Sprintf("crash on model turn %d", turn), func(t *testing.T) {
			ctx := context.Background()
			manifest := billingManifest(t)
			s := openStore(ctx, t)
			clock := newClock()
			run := seedRun(ctx, t, s, manifest, "")
			if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
				t.Fatal(err)
			}

			calls := 0
			crashing := func(execCtx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
				runCtx, cancel := context.WithCancel(execCtx)
				defer cancel()
				fence := lease.Fence()
				runner := agent.Runner{
					Store:    s,
					Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &fence, Clock: clock.Clock()},
					Manifest: manifest,
					Provider: crashAtProvider{inner: agent.ScriptedProvider{}, calls: &calls, at: turn, cancel: cancel},
					Fence:    &fence,
					Clock:    clock.Clock(),
				}
				return runner.Execute(runCtx, lease.RunID, entry.MaxSteps)
			}
			a := New(s, crashing, Config{ID: "worker-a", LeaseTTL: time.Minute, MaxAttempts: 99, Clock: clock.Clock(), Logger: quietLogger()})
			if err := a.Register(ctx); err != nil {
				t.Fatal(err)
			}
			doomedLease, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, clock.Now())
			if err != nil {
				t.Fatalf("worker-a claim: %v", err)
			}
			_, _ = crashing(ctx, doomedLease, store.QueueEntry{RunID: run.ID, MaxSteps: 10})

			crashed, err := s.Run(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if crashed.Status == "completed" {
				t.Skipf("the fixture finished before model turn %d; nothing to recover", turn)
			}

			// The dead worker's lease lapses; worker B takes the run over.
			clock.Advance(2 * time.Minute)
			b := New(s, realExecutor(s, manifest, clock.Clock()), Config{
				ID: "worker-b", LeaseTTL: time.Minute, Clock: clock.Clock(), Logger: quietLogger(),
			})
			if err = b.Register(ctx); err != nil {
				t.Fatal(err)
			}
			outcome, err := b.ClaimAndRun(ctx)
			if err != nil {
				t.Fatalf("worker-b claim: %v", err)
			}
			if outcome.Disposition != Finished {
				t.Fatalf("recovery outcome=%+v", outcome)
			}
			if outcome.Fence <= doomedLease.Token {
				t.Fatalf("recovery fence %d did not exceed the dead worker's %d", outcome.Fence, doomedLease.Token)
			}
			if !outcome.Takeover {
				t.Fatal("recovery was not reported as a takeover")
			}

			completed, err := s.Run(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if completed.Status != "completed" {
				t.Fatalf("recovered run status=%s", completed.Status)
			}
			if refunds := countRefunds(ctx, t, s, completed.WorldID); refunds != 1 {
				t.Fatalf("recovered run committed %d refunds, want exactly 1", refunds)
			}
			report, err := eval.DuplicateCharge(ctx, s, completed.WorldID)
			if err != nil {
				t.Fatal(err)
			}
			if !report.Passed {
				t.Fatalf("evaluation after recovery=%+v", report)
			}

			// The zombie must stay fenced out.
			staleFence := doomedLease.Fence()
			stale := &dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &staleFence, Clock: clock.Clock()}
			if _, err = stale.Invoke(ctx, run.ID, "call-zombie", "createRefund", refundArgs()); !errors.Is(err, store.ErrFenced) {
				t.Fatalf("zombie worker commit err=%v want ErrFenced", err)
			}

			verification, err := replay.Verify(ctx, s, run.ID, manifest)
			if err != nil {
				t.Fatalf("replay of a recovered run: %v", err)
			}
			if !verification.Verified {
				t.Fatalf("recovered run does not replay: %+v", verification)
			}
		})
	}
}

// TestInterruptedToolTransactionLeavesNothingBehind is the transactional half of
// crash recovery: a tool call killed while its transaction is open must leave no
// mutation, no ledger event and no saved result, so the retry that follows is a
// first attempt rather than a repair job.
func TestInterruptedToolTransactionLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	lease, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	fence := lease.Fence()
	d := &dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &fence, Clock: clock.Clock()}
	if _, err = d.Invoke(cancelled, run.ID, "call-refund", "createRefund", refundArgs()); err == nil {
		t.Fatal("an interrupted dispatch reported success")
	}

	current, err := s.Run(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refunds := countRefunds(ctx, t, s, current.WorldID); refunds != 0 {
		t.Fatalf("interrupted dispatch committed %d refunds", refunds)
	}
	after, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("interrupted dispatch appended %d events", len(after)-len(before))
	}
	var saved int
	if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM tool_results WHERE run_id=?", run.ID).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if saved != 0 {
		t.Fatalf("interrupted dispatch saved %d tool results", saved)
	}

	// The same call ID must now execute for real, not be answered from a
	// half-written cache.
	result, err := d.Invoke(ctx, run.ID, "call-refund", "createRefund", refundArgs())
	if err != nil {
		t.Fatalf("retry after interruption: %v", err)
	}
	if result.Status < 200 || result.Status >= 300 {
		t.Fatalf("retry status=%d", result.Status)
	}
	if refunds := countRefunds(ctx, t, s, current.WorldID); refunds != 1 {
		t.Fatalf("after retry refunds=%d want 1", refunds)
	}
}

func refundArgs() map[string]any {
	return map[string]any{"charge_id": "CH-1002", "amount_cents": 500, "reason": "duplicate charge"}
}

func countRefunds(ctx context.Context, t *testing.T, s *store.Store, worldID string) int {
	t.Helper()
	var count int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", worldID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
