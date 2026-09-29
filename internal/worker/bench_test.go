package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"twinwright/internal/pgtest"
	"twinwright/internal/store"
)

// Benchmarks for the distributed runtime's queue: how fast one worker turns a
// queued run into a finished one, and what happens to that rate as workers are
// added.
//
// The execute function is deliberately a no-op. This measures claiming,
// fencing, lease renewal setup and completion - the machinery Twinwright owns -
// not the model or tool latency of a real run, which would dominate the number
// and tell you about a fixture instead of about the runtime.
//
// The scaling shape is the point rather than the absolute figure. SQLite opens
// the store with SetMaxOpenConns(1) and serialises every writer, so adding
// workers cannot add throughput there; PostgreSQL claims under SKIP LOCKED with
// real concurrent connections. Running both is how that difference stops being
// an assumption.

type benchDialect struct {
	name string
	open func(testing.TB) *store.Store
}

func benchDialects() []benchDialect {
	list := []benchDialect{{
		name: "sqlite",
		open: func(tb testing.TB) *store.Store {
			s, err := store.Open(tb.TempDir() + "/bench.db")
			if err != nil {
				tb.Fatalf("open sqlite: %v", err)
			}
			tb.Cleanup(func() { s.Close() })
			return s
		},
	}}
	if pgtest.Available() {
		list = append(list, benchDialect{
			name: "postgres",
			open: func(tb testing.TB) *store.Store {
				s, err := store.OpenDSN(context.Background(), pgtest.SchemaDSN(tb))
				if err != nil {
					tb.Fatalf("open postgres: %v", err)
				}
				tb.Cleanup(func() { s.Close() })
				return s
			},
		})
	}
	return list
}

// enqueueRuns creates and enqueues count runs, untimed.
func enqueueRuns(tb testing.TB, s *store.Store, count int) {
	tb.Helper()
	ctx := context.Background()
	world, err := s.Seed(ctx, 42, "bench-digest")
	if err != nil {
		tb.Fatalf("seed: %v", err)
	}
	now := time.Now().UTC()
	for i := 0; i < count; i++ {
		run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "bench", "")
		if err != nil {
			tb.Fatalf("create run: %v", err)
		}
		if err = s.Enqueue(ctx, run.ID, 20, now); err != nil {
			tb.Fatalf("enqueue: %v", err)
		}
	}
}

// BenchmarkQueueDrain measures how long one queued run takes to go from
// claimable to finished, with a varying number of competing workers.
func BenchmarkQueueDrain(b *testing.B) {
	noop := func(ctx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		return store.Run{ID: lease.RunID, Status: "completed"}, nil
	}
	for _, dialect := range benchDialects() {
		for _, workers := range []int{1, 2, 4, 8} {
			b.Run(fmt.Sprintf("%s/workers=%d", dialect.name, workers), func(b *testing.B) {
				ctx := context.Background()
				s := dialect.open(b)
				enqueueRuns(b, s, b.N)
				fleet := make([]*Worker, workers)
				for i := range fleet {
					fleet[i] = New(s, noop, Config{
						ID:       fmt.Sprintf("bench-worker-%d", i),
						LeaseTTL: time.Minute,
						Logger:   quietLogger(),
					})
					if err := fleet[i].Register(ctx); err != nil {
						b.Fatalf("register: %v", err)
					}
				}

				var drained int64
				var mu sync.Mutex
				var wg sync.WaitGroup
				b.ResetTimer()
				for _, w := range fleet {
					wg.Add(1)
					go func(w *Worker) {
						defer wg.Done()
						for {
							outcome, err := w.ClaimAndRun(ctx)
							if errors.Is(err, store.ErrNoWork) {
								return
							}
							if err != nil {
								b.Errorf("claim and run: %v", err)
								return
							}
							if outcome.Disposition != Finished {
								b.Errorf("disposition=%s err=%s", outcome.Disposition, outcome.Err)
								return
							}
							mu.Lock()
							drained++
							mu.Unlock()
						}
					}(w)
				}
				wg.Wait()
				b.StopTimer()
				if drained != int64(b.N) {
					b.Fatalf("drained=%d want %d", drained, b.N)
				}
			})
		}
	}
}

// BenchmarkClaimAndFinishOneRun measures a single worker's full claim cycle
// without contention, which is the floor on per-run overhead the runtime adds
// on top of whatever the agent itself costs.
func BenchmarkClaimAndFinishOneRun(b *testing.B) {
	noop := func(ctx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		return store.Run{ID: lease.RunID, Status: "completed"}, nil
	}
	for _, dialect := range benchDialects() {
		b.Run(dialect.name, func(b *testing.B) {
			ctx := context.Background()
			s := dialect.open(b)
			enqueueRuns(b, s, b.N)
			w := New(s, noop, Config{ID: "bench-worker", LeaseTTL: time.Minute, Logger: quietLogger()})
			if err := w.Register(ctx); err != nil {
				b.Fatalf("register: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				outcome, err := w.ClaimAndRun(ctx)
				if err != nil {
					b.Fatalf("claim and run: %v", err)
				}
				if outcome.Disposition != Finished {
					b.Fatalf("disposition=%s err=%s", outcome.Disposition, outcome.Err)
				}
			}
		})
	}
}
