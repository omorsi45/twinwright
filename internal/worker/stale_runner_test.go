package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"twinwright/internal/agent"
	"twinwright/internal/compiler"
	"twinwright/internal/crashpoint"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

// The runner's own durable writes are fenced: the model request, the model turn,
// the provider failure, and the completed, paused and failed statuses. The
// dispatcher's writes have long had tests that present a stale fence. These tests
// do the same for the runner, so that dropping the fence from a runner call
// (leaving the dispatcher fenced) fails a test instead of passing every suite.

// snapshot is what a stale writer must not change: the run row and the ledger.
type snapshot struct {
	status     string
	step       int
	transcript string
	events     int
	lastType   string
}

func takeSnapshot(ctx context.Context, t *testing.T, s *store.Store, runID string) snapshot {
	t.Helper()
	run, err := s.Run(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	snap := snapshot{status: run.Status, step: run.Step, transcript: run.Transcript, events: len(events)}
	if len(events) > 0 {
		snap.lastType = events[len(events)-1].Type
	}
	return snap
}

type blockingOnce struct {
	inner   agent.Provider
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingOnce) Next(ctx context.Context, task string, history []agent.Message, ops []compiler.Operation) (agent.Message, error) {
	first := false
	b.once.Do(func() { first = true })
	if first {
		close(b.entered)
		<-b.release
	}
	return b.inner.Next(ctx, task, history, ops)
}

// TestStaleRunnerCannotCommitAModelTurnAfterTakeover is the realistic form: a
// worker stalls inside a model call (a paused process, whose renewal goroutine
// never ran), its lease lapses, a second worker takes over and completes the
// run, then the first worker's model call returns. Its turn write must be
// refused and the completed run left exactly as the new owner left it.
func TestStaleRunnerCannotCommitAModelTurnAfterTakeover(t *testing.T) {
	ctx := context.Background()
	manifest := billingManifest(t)
	s := openStore(ctx, t)
	clock := newClock()
	run := seedRun(ctx, t, s, manifest, "")
	if err := s.Enqueue(ctx, run.ID, 10, clock.Now()); err != nil {
		t.Fatal(err)
	}
	staleLease, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	fence := staleLease.Fence()
	slow := &blockingOnce{inner: agent.ScriptedProvider{}, entered: make(chan struct{}), release: make(chan struct{})}
	stale := agent.Runner{
		Store:    s,
		Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &fence, Clock: clock.Clock()},
		Manifest: manifest, Provider: slow, Fence: &fence, Clock: clock.Clock(),
	}
	staleDone := make(chan error, 1)
	go func() {
		_, err := stale.Execute(ctx, run.ID, 10)
		staleDone <- err
	}()
	<-slow.entered

	clock.Advance(2 * time.Minute)
	b := New(s, realExecutor(s, manifest, clock.Clock()), Config{ID: "worker-b", LeaseTTL: time.Minute, Clock: clock.Clock(), Logger: quietLogger()})
	if err = b.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := b.ClaimAndRun(ctx)
	if err != nil || outcome.Disposition != Finished || outcome.RunStatus != "completed" {
		t.Fatalf("takeover: outcome=%+v err=%v", outcome, err)
	}
	before := takeSnapshot(ctx, t, s, run.ID)

	close(slow.release)
	if staleErr := <-staleDone; !errors.Is(staleErr, store.ErrFenced) {
		t.Errorf("stale runner err=%v want ErrFenced", staleErr)
	}
	if after := takeSnapshot(ctx, t, s, run.ID); after != before {
		t.Errorf("stale runner changed the completed run: %+v then %+v", before, after)
	}
}

// failingProvider and fixedProvider put a chosen result into the one model turn a
// stale runner is about to record.
type failingProvider struct{ err error }

func (p failingProvider) Next(context.Context, string, []agent.Message, []compiler.Operation) (agent.Message, error) {
	return agent.Message{}, p.err
}

type fixedProvider struct{ message agent.Message }

func (p fixedProvider) Next(context.Context, string, []agent.Message, []compiler.Operation) (agent.Message, error) {
	return p.message, nil
}

// TestStaleRunnerWritesNothingAfterTakeover reaches each of the runner's durable
// writes with a fence that was superseded a moment earlier, and requires that the
// write is refused and the run and ledger stay exactly as the new owner left them.
//
// The takeover happens inside a crash-point hook, which only observes here (it
// never returns an error): the hook runs between the last write the runner made
// under a live fence and the write under test, so no goroutine or sleep is
// involved. Each case names the runner call it is meant to reach; a mutation that
// removes the fence from that one call fails that case.
func TestStaleRunnerWritesNothingAfterTakeover(t *testing.T) {
	cases := []struct {
		name string
		// runnerPoint and dispatchPoint are where the takeover happens.
		runnerPoint, dispatchPoint crashpoint.Point
		provider                   agent.Provider
		maxSteps                   int
	}{
		{name: "model request", runnerPoint: crashpoint.BeforeModelRequest, provider: agent.ScriptedProvider{}, maxSteps: 10},
		{name: "model turn", runnerPoint: crashpoint.AfterModelRequest, provider: agent.ScriptedProvider{}, maxSteps: 10},
		{name: "provider failure", runnerPoint: crashpoint.AfterModelRequest, provider: failingProvider{err: errors.New("provider unavailable")}, maxSteps: 10},
		{name: "invalid turn", runnerPoint: crashpoint.AfterModelRequest, provider: fixedProvider{message: agent.Message{Role: "user", Content: "not an assistant"}}, maxSteps: 10},
		{name: "dispatch error", runnerPoint: crashpoint.AfterModelResponse, provider: agent.ScriptedProvider{}, maxSteps: 10},
		{name: "paused status", dispatchPoint: crashpoint.AfterToolCommit, provider: agent.ScriptedProvider{}, maxSteps: 1},
		{name: "completed status", runnerPoint: crashpoint.BeforeCompletion, provider: agent.ScriptedProvider{}, maxSteps: 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			manifest := billingManifest(t)
			s := openStore(ctx, t)
			clock := newClock()
			run := seedRun(ctx, t, s, manifest, "")
			if err := s.Enqueue(ctx, run.ID, tc.maxSteps, clock.Now()); err != nil {
				t.Fatal(err)
			}
			lease, _, err := s.ClaimRun(ctx, "worker-a", time.Minute, clock.Now())
			if err != nil {
				t.Fatal(err)
			}

			var tookOver bool
			var before snapshot
			takeover := func() {
				if tookOver {
					return
				}
				tookOver = true
				clock.Advance(2 * time.Minute)
				if _, acquireErr := s.AcquireRunLease(ctx, run.ID, "worker-b", time.Minute, clock.Now()); acquireErr != nil {
					t.Error(acquireErr)
				}
				before = takeSnapshot(ctx, t, s, run.ID)
			}
			hookAt := func(point crashpoint.Point) crashpoint.Hook {
				return func(_ context.Context, site crashpoint.Site) error {
					if point != "" && site.Point == point {
						takeover()
					}
					return nil
				}
			}

			fence := lease.Fence()
			stale := agent.Runner{
				Store:    s,
				Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &fence, Clock: clock.Clock(), Crash: hookAt(tc.dispatchPoint)},
				Manifest: manifest, Provider: tc.provider, Fence: &fence, Clock: clock.Clock(), Crash: hookAt(tc.runnerPoint),
			}
			_, execErr := stale.Execute(ctx, run.ID, tc.maxSteps)

			if !tookOver {
				t.Fatal("the run never reached the point where the takeover happens, so no write was tested")
			}
			if !errors.Is(execErr, store.ErrFenced) {
				t.Errorf("stale runner err=%v want ErrFenced", execErr)
			}
			if after := takeSnapshot(ctx, t, s, run.ID); after != before {
				t.Errorf("a write by a superseded fence went through:\n before %+v\n after  %+v", before, after)
			}
		})
	}
}
