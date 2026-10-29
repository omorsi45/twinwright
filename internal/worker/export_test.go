package worker

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"twinwright/internal/store"
	"twinwright/internal/trace"
)

// lockedBuffer is a log sink a test can read while the worker's export goroutine
// may still write to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// blockedExports is an ExportTrace hook that holds every export until release is
// closed or the export's own context ends, the way a collector that does not
// answer does.
type blockedExports struct {
	release chan struct{}
	started chan context.Context
	calls   atomic.Int32
}

func newBlockedExports() *blockedExports {
	return &blockedExports{release: make(chan struct{}), started: make(chan context.Context, 8)}
}

func (b *blockedExports) hook(ctx context.Context, _ string) error {
	b.calls.Add(1)
	b.started <- ctx
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// awaitStart waits until an export has begun and returns its context.
func (b *blockedExports) awaitStart(t *testing.T) context.Context {
	t.Helper()
	select {
	case ctx := <-b.started:
		return ctx
	case <-time.After(10 * time.Second):
		t.Fatal("the export hook was never called")
		return nil
	}
}

// exportingWorker returns a worker over a store holding runs completed runs, each
// finishing at once, that exports through hook.
func exportingWorker(ctx context.Context, t *testing.T, runs int, hook func(context.Context, string) error, logs *lockedBuffer, pollInterval time.Duration) *Worker {
	t.Helper()
	s := openStore(ctx, t)
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for i := 0; i < runs; i++ {
		queuedRunForTiming(ctx, t, s, base)
	}
	execute := func(_ context.Context, lease store.RunLease, _ store.QueueEntry) (store.Run, error) {
		return store.Run{ID: lease.RunID, Status: "completed"}, nil
	}
	w := New(s, execute, Config{
		ID: "worker-a", LeaseTTL: time.Minute, PollInterval: pollInterval, Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})), ExportTrace: hook,
	})
	if err := w.Register(ctx); err != nil {
		t.Fatal(err)
	}
	return w
}

// A collector that does not answer used to hold the worker for the whole export
// bound after every completed run, so a fleet of workers with fast runs fell to
// six runs a minute each and nothing in the metrics said why. The export now runs
// behind the claim loop. One may be in flight; a run that finishes while it is
// still running is not exported, and the log names its trace so it can be sent
// by hand.
func TestExportDoesNotDelayTheNextClaim(t *testing.T) {
	ctx := context.Background()
	exports := newBlockedExports()
	var logs lockedBuffer
	w := exportingWorker(ctx, t, 3, exports.hook, &logs, 0)
	t.Cleanup(func() {
		select {
		case <-exports.release:
		default:
			close(exports.release)
		}
		w.Close()
	})

	begin := time.Now()
	first, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(begin); elapsed > 5*time.Second {
		t.Fatalf("the first claim took %v: it waited for an export that never answers", elapsed)
	}
	exports.awaitStart(t)
	if first.Disposition != Finished || !first.TraceExportQueued {
		t.Fatalf("first outcome=%+v, want a finished claim with its export queued", first)
	}

	// The export is still running, so the next run is claimed and finished at once.
	begin = time.Now()
	second, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(begin); elapsed > 5*time.Second {
		t.Fatalf("the second claim took %v", elapsed)
	}
	if second.Disposition != Finished || second.RunID == first.RunID {
		t.Fatalf("second outcome=%+v", second)
	}
	if second.TraceExportQueued {
		t.Fatalf("a second export was queued while the first was in flight: %+v", second)
	}
	if got := exports.calls.Load(); got != 1 {
		t.Fatalf("the hook ran %d times, want 1: at most one export is in flight", got)
	}
	dropped := ""
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "trace export dropped") {
			dropped = line
		}
	}
	for _, want := range []string{"level=WARN", "worker=worker-a", "run=" + second.RunID, "trace_id=" + trace.TraceIDFor(second.RunID)} {
		if !strings.Contains(dropped, want) {
			t.Fatalf("the drop warning lacks %q:\n%s", want, logs.String())
		}
	}
	if strings.Contains(dropped, trace.TraceIDFor(first.RunID)) {
		t.Fatalf("the drop warning names the run whose export was kept: %s", dropped)
	}

	// Once the export ends the slot is free again.
	close(exports.release)
	w.drainExport()
	third, err := w.ClaimAndRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !third.TraceExportQueued {
		t.Fatalf("third outcome=%+v, want its export queued now that none is in flight", third)
	}
	w.drainExport()
	if got := exports.calls.Load(); got != 2 {
		t.Fatalf("the hook ran %d times, want 2", got)
	}
}

// A stop request ends the claim loop, not an export that is already running: the
// run it belongs to is recorded and finished, and its trace is the one thing left
// to deliver. Close waits for it, and the stop does not cancel it.
func TestStopDoesNotCancelTheExportInFlight(t *testing.T) {
	exports := newBlockedExports()
	var logs lockedBuffer
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	w := exportingWorker(ctx, t, 1, exports.hook, &logs, 0)

	if _, err := w.ClaimAndRun(ctx); err != nil {
		t.Fatal(err)
	}
	exportCtx := exports.awaitStart(t)
	stop()
	if err := exportCtx.Err(); err != nil {
		t.Fatalf("the stop request cancelled the export in flight: %v", err)
	}
	if _, ok := exportCtx.Deadline(); !ok {
		t.Fatal("the export has no deadline, so a stuck collector could hold shutdown")
	}

	closed := make(chan struct{})
	go func() { w.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned while the export was still running")
	case <-time.After(150 * time.Millisecond):
	}
	close(exports.release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the export ended")
	}
}

// Close waits for the export in flight, at most exportDrainTimeout (the export
// bound), so a hook that ignores its context cannot hold shutdown for good.
func TestCloseGivesUpOnAnExportThatIgnoresItsContext(t *testing.T) {
	previous := exportDrainTimeout
	exportDrainTimeout = 50 * time.Millisecond
	t.Cleanup(func() { exportDrainTimeout = previous })

	ctx := context.Background()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	hook := func(context.Context, string) error {
		started <- struct{}{}
		<-release // ignores the context it was given
		return nil
	}
	var logs lockedBuffer
	w := exportingWorker(ctx, t, 1, hook, &logs, 0)
	t.Cleanup(func() { close(release) })
	if _, err := w.ClaimAndRun(ctx); err != nil {
		t.Fatal(err)
	}
	<-started

	begin := time.Now()
	w.Close()
	if elapsed := time.Since(begin); elapsed > 5*time.Second {
		t.Fatalf("Close took %v with an export that never ends", elapsed)
	}
	if !strings.Contains(logs.String(), "trace export still running at shutdown") {
		t.Fatalf("abandoning the export was not logged:\n%s", logs.String())
	}
}

// Serve is the worker's whole life, so it ends it properly: it does not return
// while the export of the last run it finished is in flight.
func TestServeWaitsForTheExportInFlightBeforeItReturns(t *testing.T) {
	exports := newBlockedExports()
	var logs lockedBuffer
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	w := exportingWorker(ctx, t, 1, exports.hook, &logs, 5*time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- w.Serve(ctx) }()
	exports.awaitStart(t)
	stop()
	select {
	case err := <-done:
		t.Fatalf("Serve returned (%v) with an export in flight", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(exports.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the export ended")
	}
}
