package store

import (
	"context"
	"testing"
	"time"

	"twinwright/internal/pgtest"

	_ "modernc.org/sqlite"
)

// Benchmarks for the durable primitives the runtime actually depends on: the
// ledger append behind every model turn and tool call, the lease acquisition
// that gates run ownership, the expired-lease takeover that is the distributed
// handoff, and the fence check that gates a guarded write.
//
// They deliberately do not measure model or tool latency. Those belong to a
// provider or to a simulated service, and mixing them in would turn a storage
// measurement into a number about a fixture.
//
// Every benchmark runs against each configured dialect, so SQLite and
// PostgreSQL results come out of one command and are comparable. PostgreSQL is
// skipped unless TWINWRIGHT_TEST_POSTGRES_DSN is set, exactly as the
// integration tests are.

type benchDialect struct {
	name string
	open func(testing.TB) *Store
}

func benchDialects() []benchDialect {
	list := []benchDialect{{
		name: "sqlite",
		open: func(tb testing.TB) *Store {
			s, err := Open(tb.TempDir() + "/bench.db")
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
			open: func(tb testing.TB) *Store {
				s, err := OpenDSN(context.Background(), pgtest.SchemaDSN(tb))
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

// benchRun seeds a world and creates one run to write against.
func benchRun(tb testing.TB, s *Store) Run {
	tb.Helper()
	ctx := context.Background()
	world, err := s.Seed(ctx, 42, "bench-digest")
	if err != nil {
		tb.Fatalf("seed: %v", err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "bench", "")
	if err != nil {
		tb.Fatalf("create run: %v", err)
	}
	return run
}

// benchPayload is representative of a tool.result payload rather than an empty
// object, so the measurement includes realistic JSON encoding work.
var benchPayload = map[string]any{
	"call_id":   "call-0001",
	"operation": "billing.createRefund",
	"status":    200,
	"response":  map[string]any{"refund_id": "rf-0001", "amount_cents": 5905, "currency": "USD"},
	"latency":   map[string]any{"simulated_ms": 12.5},
}

// BenchmarkAppendEvent measures ledger append throughput: the single write the
// runtime performs most often.
func BenchmarkAppendEvent(b *testing.B) {
	for _, dialect := range benchDialects() {
		b.Run(dialect.name, func(b *testing.B) {
			ctx := context.Background()
			s := dialect.open(b)
			run := benchRun(b, s)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.Append(ctx, run.ID, "tool.result", benchPayload); err != nil {
					b.Fatalf("append: %v", err)
				}
			}
		})
	}
}

// BenchmarkAppendEventGuarded is the same append under a fence. Comparing it
// against BenchmarkAppendEvent is the cost of distributed ownership checking,
// which is the number worth knowing before deciding the check is expensive.
func BenchmarkAppendEventGuarded(b *testing.B) {
	for _, dialect := range benchDialects() {
		b.Run(dialect.name, func(b *testing.B) {
			ctx := context.Background()
			s := dialect.open(b)
			run := benchRun(b, s)
			now := time.Now().UTC()
			lease, err := s.AcquireRunLease(ctx, run.ID, "bench-worker", time.Hour, now)
			if err != nil {
				b.Fatalf("acquire: %v", err)
			}
			fence := lease.Fence()
			clock := Clock(func() time.Time { return now })
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.AppendGuarded(ctx, &fence, clock, run.ID, "tool.result", benchPayload); err != nil {
					b.Fatalf("guarded append: %v", err)
				}
			}
		})
	}
}

// BenchmarkEventsRead measures reading a whole run ledger, the path replay,
// trace derivation and checkpoint listing all start from. The ledger length is
// fixed so the per-operation cost is comparable across dialects.
func BenchmarkEventsRead(b *testing.B) {
	const events = 500
	for _, dialect := range benchDialects() {
		b.Run(dialect.name, func(b *testing.B) {
			ctx := context.Background()
			s := dialect.open(b)
			run := benchRun(b, s)
			for i := 0; i < events; i++ {
				if err := s.Append(ctx, run.ID, "tool.result", benchPayload); err != nil {
					b.Fatalf("seed ledger: %v", err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err := s.Events(ctx, run.ID)
				if err != nil {
					b.Fatalf("events: %v", err)
				}
				if len(got) < events {
					b.Fatalf("events=%d want at least %d", len(got), events)
				}
			}
		})
	}
}

// BenchmarkAcquireRunLease measures uncontended lease acquisition latency. The
// release is untimed, so the reported figure is one acquisition.
func BenchmarkAcquireRunLease(b *testing.B) {
	for _, dialect := range benchDialects() {
		b.Run(dialect.name, func(b *testing.B) {
			ctx := context.Background()
			s := dialect.open(b)
			run := benchRun(b, s)
			now := time.Now().UTC()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				lease, err := s.AcquireRunLease(ctx, run.ID, "bench-worker", time.Minute, now)
				if err != nil {
					b.Fatalf("acquire: %v", err)
				}
				b.StopTimer()
				if err := s.ReleaseRunLease(ctx, lease, now); err != nil {
					b.Fatalf("release: %v", err)
				}
				b.StartTimer()
			}
		})
	}
}

// BenchmarkRenewRunLease measures the renewal a worker performs on a timer for
// the whole life of a claim.
func BenchmarkRenewRunLease(b *testing.B) {
	for _, dialect := range benchDialects() {
		b.Run(dialect.name, func(b *testing.B) {
			ctx := context.Background()
			s := dialect.open(b)
			run := benchRun(b, s)
			now := time.Now().UTC()
			lease, err := s.AcquireRunLease(ctx, run.ID, "bench-worker", time.Hour, now)
			if err != nil {
				b.Fatalf("acquire: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				lease, err = s.RenewRunLease(ctx, lease, time.Hour, now)
				if err != nil {
					b.Fatalf("renew: %v", err)
				}
			}
		})
	}
}

// BenchmarkLeaseTakeover measures the distributed handoff primitive: the cost
// for a second worker to take a run whose owner's lease has lapsed, including
// issuing a strictly higher fence. This is the storage half of recovery time
// after a worker dies; the other half is the lease TTL, which is a
// configuration choice rather than something to measure.
func BenchmarkLeaseTakeover(b *testing.B) {
	for _, dialect := range benchDialects() {
		b.Run(dialect.name, func(b *testing.B) {
			ctx := context.Background()
			s := dialect.open(b)
			run := benchRun(b, s)
			base := time.Now().UTC()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				// A dead owner: it holds the lease and never renews it.
				at := base.Add(time.Duration(i) * time.Hour)
				if _, err := s.AcquireRunLease(ctx, run.ID, "dead-worker", time.Minute, at); err != nil {
					b.Fatalf("stale acquire: %v", err)
				}
				expired := at.Add(2 * time.Minute)
				b.StartTimer()
				taken, err := s.AcquireRunLease(ctx, run.ID, "live-worker", time.Minute, expired)
				if err != nil {
					b.Fatalf("takeover: %v", err)
				}
				b.StopTimer()
				if taken.Owner != "live-worker" {
					b.Fatalf("owner=%q", taken.Owner)
				}
				if err := s.ReleaseRunLease(ctx, taken, expired); err != nil {
					b.Fatalf("release: %v", err)
				}
				b.StartTimer()
			}
		})
	}
}

// BenchmarkClaimRun measures the work-claiming query a worker runs on every
// poll: pick one runnable entry, lease it, and mark it running. On PostgreSQL
// this is the SKIP LOCKED path, which is why it is measured on both dialects
// rather than assumed to behave the same.
func BenchmarkClaimRun(b *testing.B) {
	for _, dialect := range benchDialects() {
		b.Run(dialect.name, func(b *testing.B) {
			ctx := context.Background()
			s := dialect.open(b)
			world, err := s.Seed(ctx, 42, "bench-digest")
			if err != nil {
				b.Fatalf("seed: %v", err)
			}
			now := time.Now().UTC()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "scripted", "fixture-v1", "bench", "")
				if err != nil {
					b.Fatalf("create run: %v", err)
				}
				if err = s.Enqueue(ctx, run.ID, 20, now); err != nil {
					b.Fatalf("enqueue: %v", err)
				}
				b.StartTimer()
				lease, _, err := s.ClaimRun(ctx, "bench-worker", time.Minute, now)
				if err != nil {
					b.Fatalf("claim: %v", err)
				}
				b.StopTimer()
				if err = s.FinishRun(ctx, lease, now); err != nil {
					b.Fatalf("finish: %v", err)
				}
				b.StartTimer()
			}
		})
	}
}

// BenchmarkGuardFence isolates the ownership check itself, without a write
// attached, so the guarded-append overhead can be attributed.
func BenchmarkGuardFence(b *testing.B) {
	for _, dialect := range benchDialects() {
		b.Run(dialect.name, func(b *testing.B) {
			ctx := context.Background()
			s := dialect.open(b)
			run := benchRun(b, s)
			now := time.Now().UTC()
			lease, err := s.AcquireRunLease(ctx, run.ID, "bench-worker", time.Hour, now)
			if err != nil {
				b.Fatalf("acquire: %v", err)
			}
			fence := lease.Fence()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.GuardFence(ctx, fence, now); err != nil {
					b.Fatalf("guard: %v", err)
				}
			}
		})
	}
}
