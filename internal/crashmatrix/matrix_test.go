package crashmatrix

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"twinwright/internal/crashpoint"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
	"twinwright/internal/worker"
)

const examplesRoot = "../../examples"

var matrixTTL = time.Minute

func loadScenarios(t *testing.T) []Scenario {
	t.Helper()
	scenarios, err := Scenarios(examplesRoot)
	if err != nil {
		t.Fatalf("scenarios: %v", err)
	}
	return scenarios
}

func scenarioNamed(t *testing.T, name string) Scenario {
	t.Helper()
	for _, sc := range loadScenarios(t) {
		if sc.Name == name {
			return sc
		}
	}
	t.Fatalf("no scenario named %q", name)
	return Scenario{}
}

func openSQLite(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "matrix.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// reduced keeps the first and last occurrence of each point, which is the
// smallest set that still crosses every boundary kind and every end of a
// repeated one.
func reduced(cases []Case) []Case {
	last := map[crashpoint.Point]int{}
	for _, c := range cases {
		if c.Occurrence > last[c.Point] {
			last[c.Point] = c.Occurrence
		}
	}
	var out []Case
	for _, c := range cases {
		if c.Occurrence == 1 || c.Occurrence == last[c.Point] {
			out = append(out, c)
		}
	}
	return out
}

// runMatrix crashes and recovers every case of every scenario. Each case gets
// its own store so one case's leftovers cannot make another pass or fail.
func runMatrix(t *testing.T, open func(*testing.T) *store.Store, short bool) {
	t.Helper()
	for _, sc := range loadScenarios(t) {
		t.Run(sc.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			ref, cases, err := Enumerate(ctx, open(t), sc)
			if err != nil {
				t.Fatalf("enumerate: %v", err)
			}
			if short {
				cases = reduced(cases)
			}
			start := time.Now()
			for _, c := range cases {
				t.Run(fmt.Sprintf("%s#%d", c.Point, c.Occurrence), func(t *testing.T) {
					result, err := RunCase(ctx, open(t), sc, ref, c)
					if err != nil {
						t.Fatalf("run case: %v", err)
					}
					if !result.Passed() {
						t.Fatal(strings.Join(result.Violations, "; "))
					}
				})
			}
			t.Logf("%s: %d cases in %s", sc.Name, len(cases), time.Since(start).Round(time.Millisecond))
		})
	}
}

func TestEnumerateCountsEveryBoundary(t *testing.T) {
	sc := scenarioNamed(t, "duplicate-charge")
	ref, cases, err := Enumerate(context.Background(), openSQLite(t), sc)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, point := range crashpoint.Points() {
		count := ref.Counts[point]
		total += count
		if point == crashpoint.AfterCompletion {
			if count != 1 {
				t.Errorf("Counts[%s]=%d want 1", point, count)
			}
			continue
		}
		if count == 0 {
			t.Errorf("Counts[%s]=0: the scenario never reaches this boundary", point)
		}
	}
	if len(cases) != total {
		t.Errorf("len(cases)=%d want sum(Counts)=%d", len(cases), total)
	}
	if ref.Mutations == 0 {
		t.Error("the reference run recorded no agent mutation")
	}
	if ref.TableRows["refunds"] != 1 {
		t.Errorf("TableRows[refunds]=%d want 1", ref.TableRows["refunds"])
	}
}

func TestCrashMatrixSQLite(t *testing.T) {
	runMatrix(t, openSQLite, testing.Short())
}

// recoveredCase crashes a case, lets the recovery worker finish it, and hands
// back everything Verify needs. extra, when set, runs inside the recovery
// worker's claim after the run finished, so it acts under that worker's live
// fence.
func recoveredCase(t *testing.T, sc Scenario, c Case, extra func(ctx context.Context, s *store.Store, clock *Clock, lease store.RunLease)) (*store.Store, Reference, CrashOutcome, worker.Outcome, string) {
	t.Helper()
	ctx := context.Background()
	ref, _, err := Enumerate(ctx, openSQLite(t), sc)
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	s := openSQLite(t)
	run, err := sc.Seed(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	clock := NewClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	if err = s.Enqueue(ctx, run.ID, sc.Target.Steps, clock.Now()); err != nil {
		t.Fatal(err)
	}
	crash, err := Crash(ctx, s, sc.Target, run.ID, c, clock, matrixTTL)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * matrixTTL)
	base := recoveryExecutor(s, sc.Target, clock)
	recovery := worker.New(s, func(ctx context.Context, lease store.RunLease, entry store.QueueEntry) (store.Run, error) {
		done, err := base(ctx, lease, entry)
		if err == nil && extra != nil {
			extra(ctx, s, clock, lease)
		}
		return done, err
	}, worker.Config{ID: "crashmatrix-recovery", LeaseTTL: matrixTTL, Clock: clock.Store(), Logger: quietLogger()})
	if err = recovery.Register(ctx); err != nil {
		t.Fatal(err)
	}
	outcome, err := recovery.ClaimAndRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s, ref, crash, outcome, run.ID
}

// TestVerifyReportsAnExtraMutation proves the mutation accounting can fail. The
// matrix passing means nothing unless a duplicated effect turns it red.
func TestVerifyReportsAnExtraMutation(t *testing.T) {
	sc := scenarioNamed(t, "duplicate-charge")
	c := Case{Point: crashpoint.AfterModelResponse, Occurrence: 1}
	s, ref, crash, outcome, runID := recoveredCase(t, sc, c, func(ctx context.Context, s *store.Store, clock *Clock, lease store.RunLease) {
		fence := lease.Fence()
		d := &dispatch.Dispatcher{Store: s, Manifest: sc.Target.Manifest, Fence: &fence, Clock: clock.Store()}
		args := map[string]any{"charge_id": "CH-1001", "amount_cents": 100, "reason": "extra refund"}
		result, err := d.Invoke(ctx, lease.RunID, "crashmatrix-extra", sc.Target.MutatingOperation, args)
		if err != nil || result.Status != 201 {
			t.Errorf("extra refund: status=%d err=%v", result.Status, err)
		}
	})

	result := Verify(context.Background(), s, sc.Target, &ref, runID, crash, outcome)
	if result.Passed() {
		t.Fatal("Verify passed a run with an extra committed mutation")
	}
	if !anyContains(result.Violations, "mutation") {
		t.Fatalf("no violation mentions the mutation count: %v", result.Violations)
	}
}

// TestVerifyReportsReplayDivergence proves the replay check can fail: a stored
// tool result that disagrees with the ledger must be reported.
func TestVerifyReportsReplayDivergence(t *testing.T) {
	sc := scenarioNamed(t, "duplicate-charge")
	c := Case{Point: crashpoint.AfterToolCommit, Occurrence: 1}
	s, ref, crash, outcome, runID := recoveredCase(t, sc, c, nil)

	var callID string
	if err := s.DB.QueryRow("SELECT call_id FROM tool_results WHERE run_id=? ORDER BY call_id LIMIT 1", runID).Scan(&callID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE tool_results SET body='{}' WHERE run_id=? AND call_id=?", runID, callID); err != nil {
		t.Fatal(err)
	}

	result := Verify(context.Background(), s, sc.Target, &ref, runID, crash, outcome)
	if result.Passed() {
		t.Fatal("Verify passed a run whose replay diverges")
	}
	if !anyContains(result.Violations, "replay") {
		t.Fatalf("no violation mentions replay: %v", result.Violations)
	}
}

// TestCrashHookCountsOnlyMatchingSites pins the counting rule: the Nth crossing
// means the Nth site that matches the case, so an Operation filter skips other
// operations' commits instead of counting them.
func TestCrashHookCountsOnlyMatchingSites(t *testing.T) {
	site := func(point crashpoint.Point, operation string) crashpoint.Site {
		return crashpoint.Site{Point: point, RunID: "R-1", OperationID: operation}
	}
	crashed := 0
	hook := CrashHook(Case{Point: crashpoint.AfterToolCommit, Occurrence: 2, Operation: "createRefund"}, func() { crashed++ })
	ctx := context.Background()
	sequence := []struct {
		site crashpoint.Site
		dies bool
	}{
		{site(crashpoint.AfterToolCommit, "getCharge"), false},
		{site(crashpoint.BeforeToolCommit, "createRefund"), false},
		{site(crashpoint.AfterToolCommit, "createRefund"), false},
		{site(crashpoint.AfterToolCommit, "getCharge"), false},
		{site(crashpoint.AfterToolCommit, "createRefund"), true},
		{site(crashpoint.AfterToolCommit, "createRefund"), false},
	}
	for i, step := range sequence {
		if err := hook(ctx, step.site); (err != nil) != step.dies {
			t.Fatalf("site %d (%s %s): err=%v want death=%v", i, step.site.Point, step.site.OperationID, err, step.dies)
		}
	}
	if crashed != 1 {
		t.Fatalf("onCrash ran %d times, want 1", crashed)
	}
}

func anyContains(list []string, needle string) bool {
	for _, item := range list {
		if strings.Contains(item, needle) {
			return true
		}
	}
	return false
}
