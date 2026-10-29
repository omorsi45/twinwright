package perf

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
	"twinwright/internal/crashmatrix"
	"twinwright/internal/crashpoint"
	"twinwright/internal/dispatch"
	"twinwright/internal/replay"
	"twinwright/internal/store"
	"twinwright/internal/worker"
)

const (
	// fleetLeaseTTL and fleetPoll are the throughput workers' settings. The TTL
	// is the worker package's own default, so no lease lapses mid-trial and
	// throughput does not include a takeover.
	fleetLeaseTTL = 30 * time.Second
	fleetPoll     = 10 * time.Millisecond

	// recoveryPoll is how often the recovering worker looks for work. It is part
	// of the recovery time it measures: a takeover cannot happen between polls.
	recoveryPoll = 100 * time.Millisecond

	recoveryWorkerID = "perf-recovery"
)

// harness holds what every workload shares: the scenario under test, the shape
// of a run of it, and the stores to run it against.
type harness struct {
	cfg      Config
	sc       crashmatrix.Scenario
	shape    runShape
	tmp      string
	postgres bool

	shareOnce   sync.Once
	sharedStore *store.Store
	sharedErr   error
}

// runShape is what a seeded run of the scenario is made of besides its world.
type runShape struct {
	provider, model, task string
}

func newHarness(ctx context.Context, cfg Config) (*harness, error) {
	scenarios, err := crashmatrix.Scenarios(cfg.Examples)
	if err != nil {
		return nil, fmt.Errorf("compile the example worlds under %s: %w", cfg.Examples, err)
	}
	var sc crashmatrix.Scenario
	found := false
	for _, candidate := range scenarios {
		if candidate.Name == "duplicate-charge" {
			sc, found = candidate, true
		}
	}
	if !found {
		return nil, errors.New("the crash matrix has no duplicate-charge scenario")
	}
	tmp, err := os.MkdirTemp("", "twinwright-perf-*")
	if err != nil {
		return nil, err
	}
	h := &harness{cfg: cfg, sc: sc, tmp: tmp, postgres: store.IsPostgresDSN(cfg.DSN)}
	if h.shape, err = h.probeShape(ctx); err != nil {
		h.close()
		return nil, err
	}
	return h, nil
}

// close releases the shared store and removes every temporary file.
func (h *harness) close() {
	if h.sharedStore != nil {
		_ = h.sharedStore.Close()
	}
	_ = os.RemoveAll(h.tmp)
}

// probeShape learns the provider, model and task the scenario gives a run by
// seeding one in a throwaway store. Perf seeds its own runs, with its own world
// seed, and has to give them exactly the task the matrix and the benchmark give
// theirs, without a copy of the string that could drift from the scenario.
func (h *harness) probeShape(ctx context.Context) (runShape, error) {
	s, cleanup, err := h.openTemp()
	if err != nil {
		return runShape{}, err
	}
	defer cleanup()
	run, err := h.sc.Seed(ctx, s)
	if err != nil {
		return runShape{}, fmt.Errorf("seed the %s scenario: %w", h.sc.Name, err)
	}
	return runShape{provider: run.Provider, model: run.Model, task: run.Task}, nil
}

// openTemp opens a fresh SQLite store in its own directory. The cleanup closes
// it and removes the directory, including the write-ahead log files beside it.
func (h *harness) openTemp() (*store.Store, func(), error) {
	dir, err := os.MkdirTemp(h.tmp, "db-*")
	if err != nil {
		return nil, nil, err
	}
	s, err := store.Open(filepath.Join(dir, "perf.db"))
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, fmt.Errorf("open a temporary SQLite store: %w", err)
	}
	return s, func() { _ = s.Close(); _ = os.RemoveAll(dir) }, nil
}

// shared returns the store recovery and replay run on: the configured backend,
// or one temporary SQLite file for the whole run when none was configured.
func (h *harness) shared(ctx context.Context) (*store.Store, error) {
	h.shareOnce.Do(func() {
		dsn := h.cfg.DSN
		if dsn == "" {
			dsn = filepath.Join(h.tmp, "perf.db")
		}
		h.sharedStore, h.sharedErr = store.OpenDSN(ctx, dsn)
	})
	return h.sharedStore, h.sharedErr
}

// isolated returns a store for one throughput trial or one crash matrix case.
// On SQLite that is a new file, so no trial inherits another's rows or its
// write-ahead log. A PostgreSQL server is one shared database: its trials run
// one after another against it, and requireIdleQueue is what guards them.
func (h *harness) isolated(ctx context.Context) (*store.Store, func(), error) {
	if h.postgres {
		s, err := h.shared(ctx)
		return s, func() {}, err
	}
	return h.openTemp()
}

// seedRun creates a run of the scenario in a world seeded with the configured
// seed.
func (h *harness) seedRun(ctx context.Context, s *store.Store) (store.Run, error) {
	world, err := s.SeedScenario(ctx, h.cfg.Seed, h.sc.Target.Manifest.Digest, h.sc.Name)
	if err != nil {
		return store.Run{}, fmt.Errorf("seed a world: %w", err)
	}
	run, err := s.CreateRunConfigured(ctx, world.ID, h.sc.Name, h.shape.provider, h.shape.model, h.shape.task, store.RunOptions{})
	if err != nil {
		return store.Run{}, fmt.Errorf("create a run: %w", err)
	}
	return run, nil
}

// seedQueued creates n runs and puts them on the queue.
func (h *harness) seedQueued(ctx context.Context, s *store.Store, n int) error {
	for range n {
		run, err := h.seedRun(ctx, s)
		if err != nil {
			return err
		}
		if err = s.Enqueue(ctx, run.ID, h.sc.Target.Steps, time.Now().UTC()); err != nil {
			return fmt.Errorf("enqueue %s: %w", run.ID, err)
		}
	}
	return nil
}

// localRunner is the unfenced single-node runner, used where no worker owns the
// run.
func (h *harness) localRunner(s *store.Store) agent.Runner {
	t := h.sc.Target
	return agent.Runner{
		Store:    s,
		Dispatch: &dispatch.Dispatcher{Store: s, Manifest: t.Manifest},
		Manifest: t.Manifest,
		Provider: t.Provider,
	}
}

// executor runs a claimed run under that claim's own fence, which is the wiring
// `twinwright worker` uses. Reproduced here instead of imported because the
// command lives in package main.
func (h *harness) executor(s *store.Store) worker.ExecuteFunc {
	t := h.sc.Target
	return func(ctx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		fence := lease.Fence()
		return agent.Runner{
			Store:    s,
			Dispatch: &dispatch.Dispatcher{Store: s, Manifest: t.Manifest, Fence: &fence},
			Manifest: t.Manifest,
			Provider: t.Provider,
			Fence:    &fence,
		}.Execute(ctx, lease.RunID, entry.MaxSteps)
	}
}

// quietLogger keeps the workers' per-claim log lines out of the command's
// output. The workers log normally everywhere else.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// requireIdleQueue fails when the queue holds work that is not finished.
// crashmatrix.CrashWith claims the oldest claimable run, and a throughput fleet
// claims everything, so on a database that already holds queued work they would
// take someone else's run and report a figure for it. Refusing is the only safe
// answer: the entry cannot be told apart from the one perf just enqueued.
func requireIdleQueue(ctx context.Context, s *store.Store) error {
	depth, err := s.QueueDepth(ctx)
	if err != nil {
		return fmt.Errorf("read the work queue: %w", err)
	}
	runnable, leased := depth[store.QueueRunnable], depth[store.QueueLeased]
	if runnable+leased > 0 {
		return fmt.Errorf("the work queue is not idle (%d runnable, %d leased entries): a measurement here would claim a run perf did not enqueue", runnable, leased)
	}
	return nil
}

// fleetResult is what one worker goroutine brought back.
type fleetResult struct {
	outcomes []worker.Outcome
	err      error
}

// runFleet starts workers on a real clock and returns how long they took to
// drain the queue. want is the number of runs the caller queued, and every one
// of them must end finished and completed: a throughput figure for a fleet that
// dropped or failed runs would measure the wrong thing.
//
// Each worker is the same worker.Worker `twinwright worker --drain` runs, and
// claims until the queue has nothing claimable. Timing starts before the first
// goroutine does and stops when the last one returns.
func runFleet(ctx context.Context, s *store.Store, workers, want int, execute worker.ExecuteFunc) (time.Duration, error) {
	results := make([]fleetResult, workers)
	var wg sync.WaitGroup
	start := time.Now()
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := worker.New(s, execute, worker.Config{
				ID:           fmt.Sprintf("perf-%d", i+1),
				LeaseTTL:     fleetLeaseTTL,
				PollInterval: fleetPoll,
				Logger:       quietLogger(),
			})
			results[i] = drain(ctx, w)
		}()
	}
	wg.Wait()
	wall := time.Since(start)

	completed := 0
	for i, result := range results {
		if result.err != nil {
			return 0, fmt.Errorf("worker perf-%d: %w", i+1, result.err)
		}
		for _, outcome := range result.outcomes {
			if outcome.Disposition != worker.Finished || outcome.RunStatus != "completed" {
				return 0, fmt.Errorf("worker %s ended run %s %s with status %q (%s), not finished and completed",
					outcome.WorkerID, outcome.RunID, outcome.Disposition, outcome.RunStatus, outcome.Err)
			}
			completed++
		}
	}
	if completed != want {
		return 0, fmt.Errorf("%d of %d runs completed", completed, want)
	}
	return wall, nil
}

// drain claims and runs until the queue has nothing claimable.
func drain(ctx context.Context, w *worker.Worker) fleetResult {
	var result fleetResult
	if result.err = w.Register(ctx); result.err != nil {
		return result
	}
	for {
		outcome, err := w.ClaimAndRun(ctx)
		if errors.Is(err, store.ErrNoWork) {
			return result
		}
		if err != nil {
			result.err = err
			return result
		}
		result.outcomes = append(result.outcomes, outcome)
	}
}

// throughput measures how fast `workers` workers drain a queue of Runs runs.
func (h *harness) throughput(ctx context.Context, workers int) (ThroughputResult, error) {
	var perSecond, wall []float64
	for trial := 1; trial <= h.cfg.Trials; trial++ {
		elapsed, err := h.throughputTrial(ctx, workers)
		if err != nil {
			return ThroughputResult{}, fmt.Errorf("throughput with %d worker(s), trial %d: %w", workers, trial, err)
		}
		perSecond = append(perSecond, float64(h.cfg.Runs)/elapsed.Seconds())
		wall = append(wall, millis(elapsed))
	}
	return ThroughputResult{
		Workers:       workers,
		Runs:          h.cfg.Runs,
		RunsPerSecond: statOf(perSecond),
		WallMS:        statOf(wall),
	}, nil
}

func (h *harness) throughputTrial(ctx context.Context, workers int) (time.Duration, error) {
	s, cleanup, err := h.isolated(ctx)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	if err = requireIdleQueue(ctx, s); err != nil {
		return 0, err
	}
	if err = h.seedQueued(ctx, s, h.cfg.Runs); err != nil {
		return 0, err
	}
	return runFleet(ctx, s, workers, h.cfg.Runs, h.executor(s))
}

// recovery measures how long a crashed worker's run waits for a second worker
// to finish it, with the given lease TTL.
func (h *harness) recovery(ctx context.Context, ttl time.Duration) (RecoveryResult, error) {
	s, err := h.shared(ctx)
	if err != nil {
		return RecoveryResult{}, err
	}
	var recovered []float64
	duplicates := 0
	for trial := 1; trial <= h.cfg.Trials; trial++ {
		elapsed, extra, err := h.recoveryTrial(ctx, s, ttl)
		if err != nil {
			return RecoveryResult{}, fmt.Errorf("recovery with a %s lease, trial %d: %w", ttl, trial, err)
		}
		recovered = append(recovered, millis(elapsed))
		duplicates += extra
	}
	return RecoveryResult{
		LeaseTTLMS:         ttl.Milliseconds(),
		RecoveryMS:         statOf(recovered),
		DuplicateMutations: duplicates,
	}, nil
}

// recoveryTrial kills a worker just after its first refund commits, then times
// a second worker's takeover and completion of the run.
//
// The crash reuses crashmatrix.CrashWith, which claims the run under a manually
// advanced clock. Its lease therefore expires a TTL after the clock's start, a
// little before the crash itself. Renewing the dead worker's lease at the crash
// instant, as a live worker's heartbeat would have, makes the expiry exactly a
// TTL after the crash, which is the relation the measurement states and keeps
// the figure from depending on how long the run took to reach its crash point.
// The renewal touches the lease row only; it adds nothing to the ledger.
func (h *harness) recoveryTrial(ctx context.Context, s *store.Store, ttl time.Duration) (time.Duration, int, error) {
	if err := requireIdleQueue(ctx, s); err != nil {
		return 0, 0, err
	}
	run, err := h.seedRun(ctx, s)
	if err != nil {
		return 0, 0, err
	}
	if err = s.Enqueue(ctx, run.ID, h.sc.Target.Steps, time.Now().UTC()); err != nil {
		return 0, 0, fmt.Errorf("enqueue %s: %w", run.ID, err)
	}
	// Registered before the crash so that bookkeeping is not part of the time.
	recoverer := worker.New(s, h.executor(s), worker.Config{
		ID:           recoveryWorkerID,
		LeaseTTL:     ttl,
		PollInterval: recoveryPoll,
		Logger:       quietLogger(),
	})
	if err = recoverer.Register(ctx); err != nil {
		return 0, 0, fmt.Errorf("register the recovering worker: %w", err)
	}

	crashAt := crashmatrix.Case{
		Point:      crashpoint.AfterToolCommit,
		Operation:  h.sc.Target.MutatingOperation,
		Occurrence: 1,
	}
	crash, err := crashmatrix.CrashWith(ctx, s, h.sc.Target, run.ID, crashAt, crashmatrix.NewClock(time.Now()), ttl, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("crash the first worker: %w", err)
	}
	died := time.Now()
	if !crash.Crashed {
		return 0, 0, fmt.Errorf("the first worker never reached %s on %s", crashAt.Point, crashAt.Operation)
	}
	if _, err = s.RenewRunLease(ctx, crash.Doomed, ttl, died); err != nil {
		return 0, 0, fmt.Errorf("pin the dead worker's lease to the crash: %w", err)
	}

	outcome, err := awaitRecovery(ctx, recoverer, ttl)
	elapsed := time.Since(died)
	if err != nil {
		return 0, 0, err
	}
	if outcome.RunStatus != "completed" {
		return 0, 0, fmt.Errorf("the recovered run is %q, not completed", outcome.RunStatus)
	}
	if !outcome.Takeover {
		return 0, 0, errors.New("the second worker's claim was not a takeover of the expired lease")
	}
	extra, err := duplicateRefunds(ctx, s, run.WorldID)
	if err != nil {
		return 0, 0, err
	}
	return elapsed, extra, nil
}

// awaitRecovery polls for the crashed run until a claim ends finished, giving
// up after five TTLs and ten seconds. A run that was requeued or fenced is tried
// again; one that exhausted its retries is final and fails the trial at once. If
// the caller's context ends first, as Ctrl-C does, the error says so and unwraps
// to the context's own, instead of describing the interruption as a run that
// never recovered.
func awaitRecovery(parent context.Context, w *worker.Worker, ttl time.Duration) (worker.Outcome, error) {
	limit := ttl*5 + 10*time.Second
	ctx, cancel := context.WithTimeout(parent, limit)
	defer cancel()
	last := "no claim"
	for {
		outcome, err := w.ClaimAndRun(ctx)
		switch {
		case err == nil && outcome.Disposition == worker.Finished:
			return outcome, nil
		case err == nil && outcome.Disposition == worker.Failed:
			return worker.Outcome{}, fmt.Errorf("the recovering worker failed run %s: %s", outcome.RunID, outcome.Err)
		case err == nil:
			last = string(outcome.Disposition)
		case errors.Is(err, store.ErrNoWork):
			// The lease has not expired yet. Waiting for it is what is measured.
		case ctx.Err() == nil:
			return worker.Outcome{}, fmt.Errorf("the recovering worker could not claim: %w", err)
		}
		select {
		case <-ctx.Done():
			if cause := parent.Err(); cause != nil {
				return worker.Outcome{}, fmt.Errorf("the wait for the run's recovery was cancelled (last claim: %s): %w", last, cause)
			}
			return worker.Outcome{}, fmt.Errorf("the run was not recovered within %s (last claim: %s)", limit, last)
		case <-time.After(recoveryPoll):
		}
	}
}

// duplicateRefunds counts the refunds a world committed beyond one. The
// scenario's agent issues exactly one, so a second row is an effect that
// recovery repeated. A world with none is an error rather than zero duplicates:
// the run the crash interrupted had already committed its refund, so a missing
// row means the measurement is not looking at what it thinks it is.
func duplicateRefunds(ctx context.Context, s *store.Store, worldID string) (int, error) {
	var refunds int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", worldID).Scan(&refunds); err != nil {
		return 0, fmt.Errorf("count refunds: %w", err)
	}
	if refunds == 0 {
		return 0, fmt.Errorf("world %s holds no refund, so there is nothing to compare duplicates against", worldID)
	}
	return refunds - 1, nil
}

// replay executes ReplayRuns runs to completion and times replay.Verify on each.
func (h *harness) replay(ctx context.Context) (ReplayResult, error) {
	s, err := h.shared(ctx)
	if err != nil {
		return ReplayResult{}, err
	}
	runner := h.localRunner(s)
	ids := make([]string, 0, h.cfg.ReplayRuns)
	for range h.cfg.ReplayRuns {
		run, err := h.seedRun(ctx, s)
		if err != nil {
			return ReplayResult{}, err
		}
		done, err := runner.Execute(ctx, run.ID, h.sc.Target.Steps)
		if err != nil {
			return ReplayResult{}, fmt.Errorf("run %s before replay: %w", run.ID, err)
		}
		if done.Status != "completed" {
			return ReplayResult{}, fmt.Errorf("run %s ended %q before replay, not completed", run.ID, done.Status)
		}
		ids = append(ids, run.ID)
	}

	var times []float64
	verified := 0
	for _, id := range ids {
		start := time.Now()
		report, err := replay.Verify(ctx, s, id, h.sc.Target.Manifest)
		elapsed := time.Since(start)
		if err != nil {
			return ReplayResult{}, fmt.Errorf("replay run %s: %w", id, err)
		}
		times = append(times, millis(elapsed))
		if report.Verified {
			verified++
		}
	}
	return ReplayResult{Runs: len(ids), Verified: verified, MS: statOf(times)}, nil
}

// crashMatrix crashes and recovers every duplicate-charge case. On SQLite each
// case gets a fresh file, as the matrix's own tests do, so one case's leftovers
// cannot make another pass or fail; on PostgreSQL the cases share the database
// and each starts only when the queue is idle.
func (h *harness) crashMatrix(ctx context.Context) (MatrixResult, error) {
	start := time.Now()
	s, cleanup, err := h.isolated(ctx)
	if err != nil {
		return MatrixResult{}, err
	}
	reference, cases, err := crashmatrix.Enumerate(ctx, s, h.sc)
	cleanup()
	if err != nil {
		return MatrixResult{}, fmt.Errorf("enumerate the %s crash cases: %w", h.sc.Name, err)
	}

	violations := 0
	for _, c := range cases {
		result, err := h.crashCase(ctx, reference, c)
		if err != nil {
			return MatrixResult{}, fmt.Errorf("crash case %s #%d: %w", c.Point, c.Occurrence, err)
		}
		violations += len(result.Violations)
	}
	return MatrixResult{
		Scenario:   h.sc.Name,
		Cases:      len(cases),
		Violations: violations,
		WallMS:     millis(time.Since(start)),
	}, nil
}

func (h *harness) crashCase(ctx context.Context, reference crashmatrix.Reference, c crashmatrix.Case) (crashmatrix.Result, error) {
	s, cleanup, err := h.isolated(ctx)
	if err != nil {
		return crashmatrix.Result{}, err
	}
	defer cleanup()
	if err = requireIdleQueue(ctx, s); err != nil {
		return crashmatrix.Result{}, err
	}
	return crashmatrix.RunCase(ctx, s, h.sc, reference, c)
}
