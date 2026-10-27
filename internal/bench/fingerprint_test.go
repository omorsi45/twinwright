package bench

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"twinwright/internal/store"
)

func runSuiteForFingerprint(t *testing.T, raw []byte, options Options) (Report, string) {
	t.Helper()
	root := filepath.Join("..", "..", "examples")
	suite, err := Parse(raw, root)
	if err != nil {
		t.Fatal(err)
	}
	options.ExamplesRoot = root
	options.WorkDir = t.TempDir()
	options.Agent = "scripted"
	options.ProviderFor = scriptedProviders
	report, err := Run(context.Background(), suite, options)
	if err != nil {
		t.Fatal(err)
	}
	return report, options.WorkDir
}

func TestFingerprintOfTheBillingRun(t *testing.T) {
	report, workDir := runSuiteForFingerprint(t, []byte(`
version: 1
suite: fingerprint
cases:
  - id: billing-duplicate
    category: reasoning
    world: billing
    scenario: duplicate-charge
    assertions: assertions/duplicate-charge.yaml
    dimensions: [task]
`), Options{})
	result := report.Cases[0]
	if result.Status != "passed" {
		t.Fatalf("case did not pass: %+v", result)
	}
	s, err := store.Open(filepath.Join(workDir, "billing-duplicate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	trajectory, mutations, err := Fingerprint(context.Background(), s, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var operations []string
	for _, step := range trajectory {
		operations = append(operations, step.Operation)
	}
	if got, want := strings.Join(operations, ","), "getCustomer,listInvoices,listCharges,createRefund"; got != want {
		t.Fatalf("trajectory operations=%s want %s", got, want)
	}
	if last := trajectory[len(trajectory)-1]; last.Status != 201 {
		t.Fatalf("last status=%d want 201", last.Status)
	}
	if mutations != 1 {
		t.Fatalf("mutations=%d want 1", mutations)
	}
	// The report carries the same fingerprint, so a gate can compare two reports
	// without reopening any database.
	if len(result.Trajectory) != len(trajectory) || result.Mutations != mutations {
		t.Fatalf("report fingerprint %+v mutations=%d differs from Fingerprint", result.Trajectory, result.Mutations)
	}
}

func TestFingerprintExcludesActorMutations(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	raw, err := os.ReadFile(filepath.Join(root, "bench", "standard.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	report, workDir := runSuiteForFingerprint(t, raw, Options{})
	var result CaseResult
	for _, c := range report.Cases {
		if c.ID == "reliability-concurrent-mutation" {
			result = c
		}
	}
	if result.RunID == "" {
		t.Fatal("reliability-concurrent-mutation did not run")
	}
	s, err := store.Open(filepath.Join(workDir, "reliability-concurrent-mutation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events, err := s.Events(context.Background(), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	stateMutations, actorMutations := 0, 0
	for _, e := range events {
		switch e.Type {
		case "state.mutation":
			stateMutations++
		case "chaos.actor_mutation":
			actorMutations++
		}
	}
	// Without a chaos actor write in the ledger this test could not tell an
	// exclusion from a count that never saw one.
	if actorMutations == 0 {
		t.Fatal("fixture produced no chaos.actor_mutation event")
	}
	if result.Mutations != stateMutations {
		t.Fatalf("mutations=%d want %d (state.mutation only; %d actor writes excluded)", result.Mutations, stateMutations, actorMutations)
	}
}

func TestTrialsRunEachLocalCaseNTimes(t *testing.T) {
	report, workDir := runSuiteForFingerprint(t, []byte(`
version: 1
suite: trials
cases:
  - id: billing-duplicate
    category: reasoning
    world: billing
    scenario: duplicate-charge
    assertions: assertions/duplicate-charge.yaml
    dimensions: [task]
    trials: 3
`), Options{})
	result := report.Cases[0]
	if result.Trials != 3 || result.Passes != 3 || result.Status != "passed" {
		t.Fatalf("trials=%d passes=%d status=%s", result.Trials, result.Passes, result.Status)
	}
	for _, name := range []string{"billing-duplicate-1.db", "billing-duplicate-2.db", "billing-duplicate-3.db"} {
		if _, err := os.Stat(filepath.Join(workDir, name)); err != nil {
			t.Errorf("trial database missing: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(workDir, "billing-duplicate.db")); err == nil {
		t.Error("a multi-trial case must not use the single-trial database name")
	}
}

// TestMultiTrialWallMSIsTheFirstTrial pins that a repeated case reports one
// trial's latency, not the sum. Summary latency percentiles read WallMS, so a
// sum would make --trials N look N times slower. With five identical trials the
// bench.Run call lasts about five trial durations, so a first-trial WallMS is
// near a fifth of the elapsed time and a summed one is near all of it. The
// threshold of half sits between the two with wide margin on either side.
func TestMultiTrialWallMSIsTheFirstTrial(t *testing.T) {
	started := time.Now()
	report, _ := runSuiteForFingerprint(t, []byte(`
version: 1
suite: trials
cases:
  - id: billing-duplicate
    category: reasoning
    world: billing
    scenario: duplicate-charge
    assertions: assertions/duplicate-charge.yaml
    dimensions: [task]
    trials: 5
`), Options{})
	elapsed := time.Since(started)
	result := report.Cases[0]
	if result.Trials != 5 || result.Status != "passed" {
		t.Fatalf("trials=%d status=%s", result.Trials, result.Status)
	}
	wall := time.Duration(result.WallMS) * time.Millisecond
	if wall*2 >= elapsed {
		t.Fatalf("WallMS=%v is at least half of the %v the five-trial run took; it looks summed over trials, want the first trial only", wall, elapsed)
	}
}

func TestOptionsTrialsApplyUnlessTheCaseSetsItsOwn(t *testing.T) {
	report, _ := runSuiteForFingerprint(t, []byte(`
version: 1
suite: trials
cases:
  - id: default-trials
    category: reasoning
    world: billing
    scenario: duplicate-charge
    assertions: assertions/duplicate-charge.yaml
    dimensions: [task]
  - id: own-trials
    category: reasoning
    world: billing
    scenario: duplicate-charge
    assertions: assertions/duplicate-charge.yaml
    dimensions: [task]
    trials: 2
`), Options{Trials: 4})
	if got := report.Cases[0]; got.Trials != 4 || got.Passes != 4 {
		t.Errorf("option trials: trials=%d passes=%d want 4/4", got.Trials, got.Passes)
	}
	if got := report.Cases[1]; got.Trials != 2 || got.Passes != 2 {
		t.Errorf("case trials must win: trials=%d passes=%d want 2/2", got.Trials, got.Passes)
	}
}

func TestSingleTrialKeepsTheOriginalDatabaseName(t *testing.T) {
	report, workDir := runSuiteForFingerprint(t, []byte(`
version: 1
suite: trials
cases:
  - id: billing-duplicate
    category: reasoning
    world: billing
    scenario: duplicate-charge
    assertions: assertions/duplicate-charge.yaml
    dimensions: [task]
`), Options{})
	if got := report.Cases[0]; got.Trials != 1 || got.Passes != 1 {
		t.Fatalf("trials=%d passes=%d want 1/1", got.Trials, got.Passes)
	}
	if _, err := os.Stat(filepath.Join(workDir, "billing-duplicate.db")); err != nil {
		t.Fatalf("single trial database: %v", err)
	}
}

func TestTrialsRefusedOutsideLocalMode(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	cases := map[string]string{
		"distributed": `
    mode: distributed
    crash_at: 2
`,
		"counterfactual": `
    mode: counterfactual
    interventions: counterfactual/ambiguous-commit.yaml
`,
	}
	for name, mode := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(`
version: 1
suite: refused
cases:
  - id: refused-case
    category: distributed
    world: billing
    scenario: duplicate-charge
    assertions: assertions/duplicate-charge.yaml
    dimensions: [task]
    trials: 2`+mode), root)
			if err == nil || !strings.Contains(err.Error(), "trials") {
				t.Fatalf("err=%v want a parse error naming trials", err)
			}
		})
	}
}

func TestTrialsMustBePositive(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	_, err := Parse([]byte(`
version: 1
suite: refused
cases:
  - id: refused-case
    category: reasoning
    world: billing
    scenario: duplicate-charge
    assertions: assertions/duplicate-charge.yaml
    dimensions: [task]
    trials: 0
`), root)
	if err == nil || !strings.Contains(err.Error(), "trials") {
		t.Fatalf("err=%v want a parse error naming trials", err)
	}
}

func TestSuccessfulViolationsAreRecordedForPromptInjection(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	raw, err := os.ReadFile(filepath.Join(root, "bench", "standard.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	report, _ := runSuiteForFingerprint(t, raw, Options{})
	got := map[string]CaseResult{}
	for _, c := range report.Cases {
		got[c.ID] = c
	}
	if n := got["security-injection-overprivileged"].SuccessfulViolations; n == 0 {
		t.Error("the overprivileged control must record its successful violations")
	}
	if n := got["security-injection-blocked"].SuccessfulViolations; n != 0 {
		t.Errorf("blocked case successful violations=%d want 0", n)
	}
	for _, id := range []string{"distributed-crash-after-refund-commit"} {
		if c := got[id]; c.RunID == "" || len(c.Trajectory) == 0 {
			t.Errorf("%s: want a fingerprint from the recovered run, got %+v", id, c.Trajectory)
		}
	}
}
