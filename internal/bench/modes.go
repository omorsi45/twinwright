package bench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"twinwright/internal/agent"
	"twinwright/internal/assertion"
	"twinwright/internal/compiler"
	"twinwright/internal/counterfactual"
	"twinwright/internal/dispatch"
	"twinwright/internal/replay"
	"twinwright/internal/store"
	"twinwright/internal/worker"
)

// Distributed and counterfactual execution modes.
//
// A local case runs the agent in process, which is what keeps the deterministic
// examples simple. These two modes exist because the runtime's two hardest
// features were invisible to its own benchmark: nothing crashed a worker, and
// nothing explained a failure.
//
// Both stay deterministic. The distributed mode drives a controlled clock rather
// than sleeping out a lease TTL, and the crash is injected at a named model turn
// rather than at whatever moment a timer happens to fire.

// benchClock is a deterministic clock. A distributed case has to watch a lease
// expire, and the alternative to controlling time is sleeping for the TTL, which
// would make the suite slow and its result dependent on scheduling.
type benchClock struct {
	mu  sync.Mutex
	now time.Time
}

func newBenchClock() *benchClock {
	return &benchClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *benchClock) Clock() store.Clock {
	return func() time.Time {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.now
	}
}

func (c *benchClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// crashAtProvider kills a run at a chosen model turn.
//
// Cancelling the context on the Nth provider call reproduces a process death at
// a precise durable boundary: the model request for turn N is already persisted
// and everything before it is committed, while the cancellation stops the runner
// from recording the failure. The ledger is left exactly as a killed process
// would leave it, ending in an unanswered model request.
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

const benchLeaseTTL = time.Minute

// benchLogger keeps worker logging out of bench output. The worker's own logging
// stays enabled in the code under test; only this harness discards it.
func benchLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// runDistributed executes one case through the worker runtime with an injected
// crash, and reports what the runtime actually did about it.
func runDistributed(ctx context.Context, s *store.Store, c Case, manifest compiler.Manifest, provider agent.Provider, run store.Run) (store.Run, DistributedResult, error) {
	var measured DistributedResult
	clock := newBenchClock()
	if err := s.Enqueue(ctx, run.ID, c.Steps, clock.Clock().Now()); err != nil {
		return run, measured, fmt.Errorf("enqueue: %w", err)
	}

	// Worker A claims the run and dies. It claims through the store rather than
	// through worker.ClaimAndRun because a crashed process does not get to
	// requeue itself: the lease simply lapses, which is the state a takeover has
	// to handle.
	doomed, _, err := s.ClaimRun(ctx, "bench-worker-a", benchLeaseTTL, clock.Clock().Now())
	if err != nil {
		return run, measured, fmt.Errorf("first claim: %w", err)
	}
	measured.CrashedFence = doomed.Token
	measured.Deliveries = 1

	calls := 0
	crashCtx, cancel := context.WithCancel(ctx)
	fence := doomed.Fence()
	crashing := agent.Runner{
		Store:    s,
		Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &fence, Clock: clock.Clock()},
		Manifest: manifest,
		Provider: crashAtProvider{inner: provider, calls: &calls, at: c.CrashAt, cancel: cancel},
		Fence:    &fence,
		Clock:    clock.Clock(),
	}
	_, _ = crashing.Execute(crashCtx, run.ID, c.Steps)
	cancel()

	crashed, err := s.Run(ctx, run.ID)
	if err != nil {
		return run, measured, err
	}
	// A crash point past the end of the fixture measures nothing, and reporting
	// it as a successful recovery would be a false positive. It is an error.
	if crashed.Status == "completed" {
		return crashed, measured, fmt.Errorf("the fixture completed before model turn %d, so no crash was injected", c.CrashAt)
	}
	measured.WorkerCrashed = true

	// The dead worker's lease lapses and worker B takes the run over.
	clock.Advance(2 * benchLeaseTTL)
	recovery := worker.New(s, func(runCtx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		liveFence := lease.Fence()
		runner := agent.Runner{
			Store:    s,
			Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &liveFence, Clock: clock.Clock()},
			Manifest: manifest,
			Provider: provider,
			Fence:    &liveFence,
			Clock:    clock.Clock(),
		}
		return runner.Execute(runCtx, lease.RunID, entry.MaxSteps)
	}, worker.Config{ID: "bench-worker-b", LeaseTTL: benchLeaseTTL, Clock: clock.Clock(), Logger: benchLogger()})
	if err = recovery.Register(ctx); err != nil {
		return crashed, measured, fmt.Errorf("register recovery worker: %w", err)
	}
	outcome, err := recovery.ClaimAndRun(ctx)
	if err != nil {
		return crashed, measured, fmt.Errorf("takeover claim: %w", err)
	}
	measured.Takeover = outcome.Takeover
	measured.RecoveryFence = outcome.Fence
	measured.Deliveries = outcome.Attempt
	if outcome.Disposition != worker.Finished {
		return crashed, measured, fmt.Errorf("takeover ended as %s: %s", outcome.Disposition, outcome.Err)
	}

	// The zombie wakes up and tries to commit. Its rejection is the property
	// fencing exists for, so it is exercised rather than assumed.
	stale := &dispatch.Dispatcher{Store: s, Manifest: manifest, Fence: &fence, Clock: clock.Clock()}
	_, staleErr := stale.Invoke(ctx, run.ID, "bench-zombie-call", zombieOperation(c.World), zombieArguments(c.World))
	switch {
	case errors.Is(staleErr, store.ErrFenced):
		measured.FencingRejections = 1
	case staleErr == nil:
		return crashed, measured, fmt.Errorf("a stale worker committed after losing the run")
	default:
		return crashed, measured, fmt.Errorf("stale commit failed for the wrong reason: %w", staleErr)
	}

	recovered, err := s.Run(ctx, run.ID)
	if err != nil {
		return crashed, measured, err
	}
	// A run that survives a crash but can no longer be audited has lost the
	// property the project exists to provide, so replay is part of the measure.
	verification, err := replay.Verify(ctx, s, run.ID, manifest)
	if err != nil {
		return recovered, measured, fmt.Errorf("replay of the recovered run: %w", err)
	}
	measured.ReplayVerified = verification.Verified
	return recovered, measured, nil
}

// zombieOperation is the mutation the stale worker attempts. It is a write, so
// that a missing fence check would show up as a committed effect rather than as
// a harmless read.
func zombieOperation(world string) string {
	if world == "company" {
		return "billing.createRefund"
	}
	return "createRefund"
}

func zombieArguments(string) map[string]any {
	return map[string]any{"charge_id": "CH-1002", "amount_cents": 500, "reason": "stale worker retry"}
}

// runCounterfactual explains a case's failure by intervention analysis.
func runCounterfactual(ctx context.Context, s *store.Store, c Case, manifest compiler.Manifest, options Options, run store.Run) (CounterfactualResult, error) {
	var measured CounterfactualResult
	path, err := Resolve(options.ExamplesRoot, c.Interventions)
	if err != nil {
		return measured, err
	}
	raw, err := os.ReadFile(filepath.Join(options.ExamplesRoot, filepath.FromSlash(path)))
	if err != nil {
		return measured, err
	}
	set, err := counterfactual.Parse(raw, manifest)
	if err != nil {
		return measured, err
	}
	judge := counterfactual.ScenarioJudge()
	if c.Assertions != "" {
		assertionPath, err := Resolve(options.ExamplesRoot, c.Assertions)
		if err != nil {
			return measured, err
		}
		rawAssertions, err := os.ReadFile(filepath.Join(options.ExamplesRoot, filepath.FromSlash(assertionPath)))
		if err != nil {
			return measured, err
		}
		parsed, err := assertion.Parse(rawAssertions, manifest, assertion.Builtins())
		if err != nil {
			return measured, err
		}
		judge = counterfactual.AssertionJudge(parsed)
	}

	// Prepare refuses a parent that passes its judge, which is the invariant
	// that keeps this case honest: an analysis of a passing run explains nothing.
	analysis, err := counterfactual.Prepare(ctx, s, run.ID, manifest, set, judge, counterfactual.Options{
		Trials: 1,
		Steps:  c.Steps,
		ProviderFor: func(provider, model, scenario string) (agent.Provider, error) {
			return options.ProviderFor(provider, model, scenario, options.BaseURL)
		},
	})
	if err != nil {
		return measured, err
	}
	report, err := analysis.Run(ctx, s, s)
	if err != nil {
		return measured, err
	}
	measured.ParentPassed = false
	measured.ParentFailed = report.Failure.Failed
	measured.Candidates = len(report.Candidates)
	for _, candidate := range report.Candidates {
		if candidate.Changed > 0 {
			measured.Explained = true
			measured.Explanation = candidate.Summary
			break
		}
	}
	return measured, nil
}
