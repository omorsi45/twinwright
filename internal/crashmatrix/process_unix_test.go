//go:build unix

package crashmatrix

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"twinwright/internal/crashpoint"
	"twinwright/internal/pgtest"
	"twinwright/internal/store"
)

// The in-process matrix cannot show what survives a dead process that holds an
// open database transaction: its "death" is a cancelled context in a process
// that is still alive, so its connections close in an orderly way. These tests
// re-execute the test binary as a worker that SIGKILLs itself at the crash
// point, so the database has to cope with a client that vanished mid-transaction,
// and then a second worker recovers the run.

const (
	helperEnv     = "TWINWRIGHT_CRASHMATRIX_HELPER"
	helperTimeout = 60 * time.Second

	// Exit codes of a helper that did not die. They are distinct so a failure
	// names whether the crash point was missed or the helper never got that far.
	helperSetupFailedExit = 2
	helperSurvivedExit    = 3

	// notAsserted marks a case with no durable-state expectation before recovery.
	notAsserted = -1
)

// TestMain turns the test binary into the doomed worker when the parent asks
// for it, so no separate helper binary has to be built.
func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(runDoomedWorker())
	}
	os.Exit(m.Run())
}

// runDoomedWorker claims the parent's run as the doomed worker and executes it
// until the crash point, where the process kills itself with SIGKILL. It never
// seeds: the parent owns the world, so the child differs from a production
// worker only in how it dies. It returns an exit code only when the kill did
// not happen, because a surviving helper means the test proved nothing.
//
// It calls CrashWith, the same claim and runner wiring the in-process matrix
// uses, with the signal as its onCrash so the death happens at the site.
func runDoomedWorker() int {
	fail := func(code int, format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "doomed worker: "+format+"\n", args...)
		return code
	}
	occurrence, err := strconv.Atoi(os.Getenv("TW_CM_OCCURRENCE"))
	if err != nil {
		return fail(helperSetupFailedExit, "TW_CM_OCCURRENCE: %v", err)
	}
	scenarios, err := Scenarios(examplesRoot)
	if err != nil {
		return fail(helperSetupFailedExit, "scenarios: %v", err)
	}
	var target *Target
	for i := range scenarios {
		if scenarios[i].Name == os.Getenv("TW_CM_SCENARIO") {
			target = &scenarios[i].Target
		}
	}
	if target == nil {
		return fail(helperSetupFailedExit, "no scenario named %q", os.Getenv("TW_CM_SCENARIO"))
	}
	ctx := context.Background()
	s, err := store.OpenDSN(ctx, os.Getenv("TW_CM_DSN"))
	if err != nil {
		return fail(helperSetupFailedExit, "open store: %v", err)
	}
	c := Case{
		Point:      crashpoint.Point(os.Getenv("TW_CM_POINT")),
		Occurrence: occurrence,
		Operation:  os.Getenv("TW_CM_OPERATION"),
	}
	_, err = CrashWith(ctx, s, *target, os.Getenv("TW_CM_RUN"), c, NewClock(time.Now()), time.Minute, func() {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {} // the signal is asynchronous; never return into the runner first
	})
	return fail(helperSurvivedExit, "survived %s #%d: CrashWith returned %v", c.Point, c.Occurrence, err)
}

// processDeathCase is one crash of a real process.
type processDeathCase struct {
	c Case
	// refunds is the number of refunds that must be durable after the process
	// died and before anything recovered it, or notAsserted.
	refunds int
}

// killedProcess runs the doomed worker as a child process and requires that it
// died of SIGKILL. A child killed by the timeout also reports SIGKILL, so the
// context is checked first: a hung helper must not read as a successful crash.
func killedProcess(t *testing.T, dsn, runID string, c Case) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), helperTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		helperEnv+"=1",
		"TW_CM_DSN="+dsn,
		"TW_CM_SCENARIO=duplicate-charge",
		"TW_CM_RUN="+runID,
		"TW_CM_POINT="+string(c.Point),
		"TW_CM_OCCURRENCE="+strconv.Itoa(c.Occurrence),
		"TW_CM_OPERATION="+c.Operation,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("doomed worker timed out after %s: %s", helperTimeout, stderr.String())
	}
	var exit *exec.ExitError
	if !errors.As(runErr, &exit) {
		t.Fatalf("doomed worker was not killed: err=%v stderr=%s", runErr, stderr.String())
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("doomed worker did not die of SIGKILL: %v stderr=%s", exit, stderr.String())
	}
	t.Logf("doomed worker at %s #%d: %v", c.Point, c.Occurrence, exit)
}

// runProcessDeath kills a real worker at each case and recovers it with a second
// one. dsn yields a fresh database location per call: one for the uncrashed
// reference run Verify compares against, and one for the run that is killed.
func runProcessDeath(t *testing.T, dsn func(*testing.T) string, cases []processDeathCase) {
	t.Helper()
	sc := scenarioNamed(t, "duplicate-charge")
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s#%d", tc.c.Point, tc.c.Occurrence), func(t *testing.T) {
			ctx := context.Background()
			ref, _, err := Enumerate(ctx, openReference(t, dsn), sc)
			if err != nil {
				t.Fatalf("enumerate: %v", err)
			}

			target := dsn(t)
			s, err := store.OpenDSN(ctx, target)
			if err != nil {
				t.Fatal(err)
			}
			run, err := sc.Seed(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Enqueue(ctx, run.ID, sc.Target.Steps, time.Now()); err != nil {
				t.Fatal(err)
			}
			// The child must be the only process using the database while it runs,
			// so the parent lets go of its connections before it starts.
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}

			killedProcess(t, target, run.ID, tc.c)

			s, err = store.OpenDSN(ctx, target)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			doomed, err := s.RunLease(ctx, run.ID)
			if err != nil {
				t.Fatalf("the dead worker left no lease: %v", err)
			}
			if doomed.Owner != doomedOwner {
				t.Fatalf("lease owner is %q, want the dead worker %q", doomed.Owner, doomedOwner)
			}
			last, err := s.LastEvent(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			crash := CrashOutcome{Doomed: doomed, CrashSeq: last.Seq, Crashed: true}

			if tc.refunds != notAsserted {
				var refunds int
				if err = s.DB.QueryRowContext(ctx, "SELECT count(*) FROM refunds WHERE world_id=?", run.WorldID).Scan(&refunds); err != nil {
					t.Fatal(err)
				}
				if refunds != tc.refunds {
					t.Fatalf("%d refund(s) are durable after the process died at %s, want %d", refunds, tc.c.Point, tc.refunds)
				}
			}

			clock := NewClock(time.Now().Add(2 * time.Minute))
			outcome, err := Recover(ctx, s, sc.Target, clock, time.Minute)
			if err != nil {
				t.Fatalf("recover: %v", err)
			}
			result := Verify(ctx, s, sc.Target, &ref, run.ID, crash, outcome)
			for _, violation := range result.Violations {
				t.Error(violation)
			}
		})
	}
}

// openReference opens a store separate from the one the child will kill itself
// against, for the uncrashed reference run.
func openReference(t *testing.T, dsn func(*testing.T) string) *store.Store {
	t.Helper()
	s, err := store.OpenDSN(context.Background(), dsn(t))
	if err != nil {
		t.Fatalf("open reference store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRealProcessDeathIsRecovered(t *testing.T) {
	runProcessDeath(t, func(t *testing.T) string {
		return t.TempDir() + "/death.db"
	}, []processDeathCase{
		{Case{Point: crashpoint.AfterModelRequest, Occurrence: 2}, notAsserted},
		{Case{Point: crashpoint.AfterModelResponse, Occurrence: 3}, notAsserted},
		{Case{Point: crashpoint.BeforeToolCommit, Occurrence: 1, Operation: "createRefund"}, 0},
		{Case{Point: crashpoint.AfterToolCommit, Occurrence: 1, Operation: "createRefund"}, 1},
	})
}

// TestRealProcessDeathIsRecoveredPostgres is the case this file exists for: a
// server holding an open transaction for a client that no longer exists must
// roll it back, and keep what was already committed.
func TestRealProcessDeathIsRecoveredPostgres(t *testing.T) {
	if !pgtest.Available() {
		t.Skipf("%s is not set; skipping PostgreSQL process death", pgtest.DSNEnv)
	}
	runProcessDeath(t, func(t *testing.T) string { return pgtest.SchemaDSN(t) }, []processDeathCase{
		{Case{Point: crashpoint.BeforeToolCommit, Occurrence: 1, Operation: "createRefund"}, 0},
		{Case{Point: crashpoint.AfterToolCommit, Occurrence: 1, Operation: "createRefund"}, 1},
	})
}
