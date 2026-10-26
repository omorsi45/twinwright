// Package crashmatrix crashes a run at every durable boundary and checks that a
// second worker recovers it.
//
// A crash test that picks its crash points by hand proves only that those points
// recover. The refund-once check that preceded this package passed trivially:
// its last crash point came before the refund committed, so it never crashed
// after the effect it claimed to protect. The matrix removes the choice. It runs
// a scenario once to count how often each boundary is reached, then crashes a
// first worker at every (boundary, occurrence), lets a second worker take over,
// and verifies the same recovery invariants each time.
//
// Delivery stays at-least-once. The matrix proves that repeating the work is
// safe, not that it never repeats.
package crashmatrix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"twinwright/internal/agent"
	"twinwright/internal/crashpoint"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
	"twinwright/internal/worker"
)

const (
	doomedOwner   = "crashmatrix-doomed"
	recoveryOwner = "crashmatrix-recovery"
)

// Case is one crash to inject: the Occurrence-th time Point is reached.
// Operation, when set, narrows the count to sites for that tool operation, which
// is how a hand-written case targets "the second createRefund commit".
type Case struct {
	Point      crashpoint.Point `json:"point"`
	Occurrence int              `json:"occurrence"`
	Operation  string           `json:"operation,omitempty"`
}

// Reference is what an uncrashed run of a scenario looks like. A recovered run
// must end up with the same effects, so it is the yardstick for duplicates.
type Reference struct {
	// Counts is how many times each fired point was reached, and 1 for
	// AfterCompletion, which no code fires.
	Counts map[crashpoint.Point]int
	// Mutations is the number of state mutations the agent committed.
	Mutations int
	// TableRows is the row count of each per-world table for the run's world.
	TableRows map[string]int
}

// Enumerate runs the scenario once without crashing and returns what it saw
// along with one Case per boundary crossing.
//
// A reference run that fails its own judge is an error rather than a baseline:
// recovery would then be compared against a result that was already wrong.
func Enumerate(ctx context.Context, s *store.Store, sc Scenario) (Reference, []Case, error) {
	ref := Reference{Counts: map[crashpoint.Point]int{}}
	run, err := sc.Seed(ctx, s)
	if err != nil {
		return ref, nil, fmt.Errorf("seed %s: %w", sc.Name, err)
	}
	var mu sync.Mutex
	counting := func(_ context.Context, site crashpoint.Site) error {
		mu.Lock()
		defer mu.Unlock()
		ref.Counts[site.Point]++
		return nil
	}
	runner := newRunner(s, sc.Target, nil, nil, counting)
	done, err := runner.Execute(ctx, run.ID, sc.Target.Steps)
	if err != nil {
		return ref, nil, fmt.Errorf("reference run of %s: %w", sc.Name, err)
	}
	if done.Status != "completed" {
		return ref, nil, fmt.Errorf("reference run of %s ended %s, not completed", sc.Name, done.Status)
	}
	passed, failed, err := sc.Judge(ctx, s, done)
	if err != nil {
		return ref, nil, fmt.Errorf("judge reference run of %s: %w", sc.Name, err)
	}
	if !passed {
		return ref, nil, fmt.Errorf("reference run of %s fails its own judge: %v", sc.Name, failed)
	}
	ref.Counts[crashpoint.AfterCompletion] = 1
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		return ref, nil, err
	}
	ref.Mutations = attributeMutations(events).total
	if ref.TableRows, err = worldRows(ctx, s, done.WorldID); err != nil {
		return ref, nil, err
	}
	var cases []Case
	for _, point := range crashpoint.Points() {
		for occurrence := 1; occurrence <= ref.Counts[point]; occurrence++ {
			cases = append(cases, Case{Point: point, Occurrence: occurrence})
		}
	}
	return ref, cases, nil
}

// Clock is a manually advanced clock. Lease expiry is what the matrix drives, and
// advancing time by hand keeps a case deterministic instead of sleeping out a
// TTL.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a clock reading start.
func NewClock(start time.Time) *Clock { return &Clock{now: start} }

// Now reports the clock's current time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Store adapts the clock to the store's clock type.
func (c *Clock) Store() store.Clock { return c.Now }

// CrashHook returns a hook that dies at the Occurrence-th site matching c, after
// calling onCrash. Sites that do not match do not count, so the Nth crossing of
// a point means the same thing in the reference run and in the crashed one.
func CrashHook(c Case, onCrash func()) crashpoint.Hook {
	var mu sync.Mutex
	seen := 0
	return func(_ context.Context, site crashpoint.Site) error {
		if site.Point != c.Point || (c.Operation != "" && site.OperationID != c.Operation) {
			return nil
		}
		mu.Lock()
		seen++
		hit := seen == c.Occurrence
		mu.Unlock()
		if !hit {
			return nil
		}
		onCrash()
		return fmt.Errorf("crash matrix: worker died at %s #%d", c.Point, c.Occurrence)
	}
}

// CrashOutcome is what the first worker left behind.
type CrashOutcome struct {
	// Doomed is the first worker's lease. Its fence must stay rejected for the
	// rest of the run, because the worker may wake up later.
	Doomed store.RunLease
	// CrashSeq is the highest ledger sequence that was durable at the crash.
	CrashSeq int
	// Crashed reports that the simulated death happened. It is false only when
	// the case's boundary was never reached, which the matrix treats as a failure
	// rather than a pass, since nothing was tested.
	Crashed bool
}

// Crash claims the run as a worker that will die, and runs it until the case's
// boundary.
//
// The hook cancels the run context as well as returning an error, so every later
// write the runner might attempt is refused, which is what process death does.
// AfterCompletion has no site in the code: nothing runs after the completed
// status is written. It is simulated by running to completion and not calling
// FinishRun, leaving a completed run whose queue entry is still leased.
func Crash(ctx context.Context, s *store.Store, t Target, runID string, c Case, clock *Clock, ttl time.Duration) (CrashOutcome, error) {
	var out CrashOutcome
	lease, _, err := s.ClaimRun(ctx, doomedOwner, ttl, clock.Now())
	if err != nil {
		return out, fmt.Errorf("claim run for the doomed worker: %w", err)
	}
	out.Doomed = lease

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var hook crashpoint.Hook
	if c.Point != crashpoint.AfterCompletion {
		hook = CrashHook(c, func() {
			out.Crashed = true
			cancel()
		})
	}
	fence := lease.Fence()
	_, execErr := newRunner(s, t, &fence, clock, hook).Execute(runCtx, runID, t.Steps)
	switch {
	case c.Point == crashpoint.AfterCompletion:
		if execErr != nil {
			return out, fmt.Errorf("run before after-completion crash: %w", execErr)
		}
		out.Crashed = true
	case out.Crashed:
		if !errors.Is(execErr, crashpoint.ErrCrashed) {
			return out, fmt.Errorf("crash at %s #%d surfaced as %v", c.Point, c.Occurrence, execErr)
		}
	case execErr != nil:
		return out, fmt.Errorf("run ended before %s #%d: %w", c.Point, c.Occurrence, execErr)
	}

	last, err := s.LastEvent(ctx, runID)
	if err != nil {
		return out, fmt.Errorf("read ledger after crash: %w", err)
	}
	out.CrashSeq = last.Seq
	return out, nil
}

// Recover lets a second worker claim the crashed run and finish it. It advances
// no time: the caller moves the clock past the doomed lease first, so the
// takeover path is the one a real expiry produces.
func Recover(ctx context.Context, s *store.Store, t Target, clock *Clock, ttl time.Duration) (worker.Outcome, error) {
	w := worker.New(s, recoveryExecutor(s, t, clock), worker.Config{
		ID: recoveryOwner, LeaseTTL: ttl, Clock: clock.Store(), Logger: quietLogger(),
	})
	if err := w.Register(ctx); err != nil {
		return worker.Outcome{}, fmt.Errorf("register recovery worker: %w", err)
	}
	return w.ClaimAndRun(ctx)
}

// recoveryExecutor runs a claimed run under that claim's own fence, the way a
// production worker does.
func recoveryExecutor(s *store.Store, t Target, clock *Clock) worker.ExecuteFunc {
	return func(ctx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		fence := lease.Fence()
		return newRunner(s, t, &fence, clock, nil).Execute(ctx, lease.RunID, entry.MaxSteps)
	}
}

// newRunner wires a runner and its dispatcher to one crash hook. A nil fence is
// the unfenced local path the reference run uses.
func newRunner(s *store.Store, t Target, fence *store.Fence, clock *Clock, hook crashpoint.Hook) agent.Runner {
	var storeClock store.Clock
	if clock != nil {
		storeClock = clock.Store()
	}
	return agent.Runner{
		Store:    s,
		Dispatch: &dispatch.Dispatcher{Store: s, Manifest: t.Manifest, Fence: fence, Clock: storeClock, Crash: hook},
		Manifest: t.Manifest,
		Provider: t.Provider,
		Fence:    fence,
		Clock:    storeClock,
		Crash:    hook,
	}
}

// quietLogger keeps worker logging out of test output. The worker's own logging
// stays enabled in the code under test; only this harness discards it.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// caseTTL and caseStart fix the timeline of every case: the doomed worker leases
// the run for a minute from this instant, and the clock then jumps two TTLs.
const caseTTL = time.Minute

var caseStart = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// RunCase crashes one case, recovers it, and verifies the result. A returned
// error means the harness itself could not run; a recovery that goes wrong is
// reported as violations on the Result so the matrix names every broken case.
func RunCase(ctx context.Context, s *store.Store, sc Scenario, ref Reference, c Case) (Result, error) {
	run, err := sc.Seed(ctx, s)
	if err != nil {
		return Result{}, fmt.Errorf("seed %s: %w", sc.Name, err)
	}
	clock := NewClock(caseStart)
	if err = s.Enqueue(ctx, run.ID, sc.Target.Steps, clock.Now()); err != nil {
		return Result{}, fmt.Errorf("enqueue: %w", err)
	}
	crash, err := Crash(ctx, s, sc.Target, run.ID, c, clock, caseTTL)
	if err != nil {
		return Result{}, err
	}
	clock.Advance(2 * caseTTL)
	outcome, err := Recover(ctx, s, sc.Target, clock, caseTTL)
	if err != nil {
		return Result{Case: c, RunID: run.ID, Crashed: crash.Crashed, CrashedFence: crash.Doomed.Token,
			Violations: []string{fmt.Sprintf("recovery worker could not claim and run the crashed run: %v", err)}}, nil
	}
	result := Verify(ctx, s, sc.Target, &ref, run.ID, crash, outcome)
	result.Case = c

	final, err := s.Run(ctx, run.ID)
	if err != nil {
		return result, fmt.Errorf("read recovered run: %w", err)
	}
	passed, failed, err := sc.Judge(ctx, s, final)
	if err != nil {
		return result, fmt.Errorf("judge recovered run: %w", err)
	}
	if !passed {
		result.Violations = append(result.Violations, fmt.Sprintf("scenario judge failed after recovery: %v", failed))
	}
	return result, nil
}
