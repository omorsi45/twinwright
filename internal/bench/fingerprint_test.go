package bench

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
			if err == nil || !strings.Contains(err.Error(), `case "refused-case": trials requires mode local`) {
				t.Fatalf("err=%v want %q", err, `case "refused-case": trials requires mode local`)
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
	if err == nil || !strings.Contains(err.Error(), `case "refused-case": trials must be positive`) {
		t.Fatalf("err=%v want %q", err, `case "refused-case": trials must be positive`)
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

func passedTrial() CaseResult {
	return CaseResult{ID: "c", Status: "passed", Passed: true}
}

func failedTrial(checks ...string) CaseResult {
	return CaseResult{ID: "c", Status: "failed", FailedChecks: checks}
}

func erroredTrial(message string) CaseResult {
	return CaseResult{ID: "c", Status: "error", Error: message}
}

// TestFoldTrialsVerdictIsIndependentOfTrialOrder pins the precedence the
// aggregate relies on. Aggregate drops errored cases from every rate, so an
// order-dependent fold would score the same three trials differently depending
// on which came first.
func TestFoldTrialsVerdictIsIndependentOfTrialOrder(t *testing.T) {
	tests := []struct {
		name         string
		trials       []CaseResult
		status       string
		passed       bool
		passes       int
		verdictTrial int
		failedChecks []string
		errText      string
	}{
		{"pass error fail", []CaseResult{passedTrial(), erroredTrial("boom"), failedTrial("refund_once")}, "failed", false, 1, 3, []string{"refund_once"}, ""},
		{"pass fail error", []CaseResult{passedTrial(), failedTrial("refund_once"), erroredTrial("boom")}, "failed", false, 1, 2, []string{"refund_once"}, ""},
		{"error pass fail", []CaseResult{erroredTrial("boom"), passedTrial(), failedTrial("refund_once")}, "failed", false, 1, 3, []string{"refund_once"}, ""},
		{"all failed names the first", []CaseResult{passedTrial(), failedTrial("a"), failedTrial("b")}, "failed", false, 1, 2, []string{"a"}, ""},
		{"all errored names the first", []CaseResult{erroredTrial("first"), erroredTrial("second")}, "error", false, 0, 1, nil, "first"},
		{"pass then error is not a pass", []CaseResult{passedTrial(), erroredTrial("boom")}, "error", false, 1, 2, nil, "boom"},
		{"error then pass is not a pass", []CaseResult{erroredTrial("boom"), passedTrial()}, "error", false, 1, 1, nil, "boom"},
		{"all passed", []CaseResult{passedTrial(), passedTrial(), passedTrial()}, "passed", true, 3, 0, nil, ""},
		{"one trial passed", []CaseResult{passedTrial()}, "passed", true, 1, 0, nil, ""},
		{"one trial failed has no verdict trial", []CaseResult{failedTrial("a")}, "failed", false, 0, 0, []string{"a"}, ""},
		{"one trial errored has no verdict trial", []CaseResult{erroredTrial("boom")}, "error", false, 0, 0, nil, "boom"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := foldTrials(tc.trials)
			if got.Trials != len(tc.trials) || got.Passes != tc.passes {
				t.Errorf("trials=%d passes=%d want %d/%d", got.Trials, got.Passes, len(tc.trials), tc.passes)
			}
			if got.Status != tc.status || got.Passed != tc.passed {
				t.Errorf("status=%q passed=%v want %q/%v", got.Status, got.Passed, tc.status, tc.passed)
			}
			if got.VerdictTrial != tc.verdictTrial {
				t.Errorf("verdict_trial=%d want %d", got.VerdictTrial, tc.verdictTrial)
			}
			if strings.Join(got.FailedChecks, ",") != strings.Join(tc.failedChecks, ",") {
				t.Errorf("failed checks=%v want %v", got.FailedChecks, tc.failedChecks)
			}
			if got.Error != tc.errText {
				t.Errorf("error=%q want %q", got.Error, tc.errText)
			}
		})
	}
}

// The trajectory, run ID and wall time are for display and come from the first
// trial. The mutation count is not: it is a safety counter, folded over every
// trial by TestFoldTrialsSafetyCountersCoverEveryTrial.
func TestFoldTrialsKeepsTheFirstTrialsTrajectoryRunIDAndWallTime(t *testing.T) {
	first := CaseResult{ID: "c", Status: "passed", Passed: true, RunID: "run-1", WallMS: 10, Mutations: 1, Trajectory: []Step{{Operation: "createRefund", Status: 201}}}
	second := CaseResult{ID: "c", Status: "passed", Passed: true, RunID: "run-2", WallMS: 20, Mutations: 2, Trajectory: []Step{{Operation: "getCustomer", Status: 200}}}
	third := CaseResult{ID: "c", Status: "passed", Passed: true, RunID: "run-3", WallMS: 30}
	got := foldTrials([]CaseResult{first, second, third})
	if got.WallMS != 10 || got.RunID != "run-1" || len(got.Trajectory) != 1 || got.Trajectory[0].Operation != "createRefund" {
		t.Fatalf("want the first trial's wall time, run ID and trajectory, got wall=%d run=%s trajectory=%v", got.WallMS, got.RunID, got.Trajectory)
	}
}

// TestFoldTrialsSafetyCountersCoverEveryTrial pins that a leak in any trial is
// visible. The gate's new_violation and extra_mutations kinds read these two
// fields, and a live-model report is gated with a tolerance that absorbs one
// lost trial, so a counter taken from trial 1 alone lets a leak in trial 2 or
// later ship green.
func TestFoldTrialsSafetyCountersCoverEveryTrial(t *testing.T) {
	clean := CaseResult{ID: "c", Status: "passed", Passed: true, Mutations: 1}
	leaked := CaseResult{ID: "c", Status: "failed", FailedChecks: []string{"successful_violation"}, Mutations: 3, SuccessfulViolations: 2}
	for name, trials := range map[string][]CaseResult{
		"leak first":  {leaked, clean, clean},
		"leak middle": {clean, leaked, clean},
		"leak last":   {clean, clean, leaked},
	} {
		t.Run(name, func(t *testing.T) {
			got := foldTrials(trials)
			if got.Mutations != 3 || got.SuccessfulViolations != 2 {
				t.Fatalf("mutations=%d successful_violations=%d want 3/2", got.Mutations, got.SuccessfulViolations)
			}
		})
	}
}

// TestFoldTrialsSafetyCountersAreIndependentOfTrialOrder folds every ordering
// of four unlike trials. A counter that depended on the order would let the
// gate's verdict depend on which trial leaked.
func TestFoldTrialsSafetyCountersAreIndependentOfTrialOrder(t *testing.T) {
	trials := []CaseResult{
		{ID: "c", Status: "passed", Passed: true, Mutations: 1},
		{ID: "c", Status: "failed", Mutations: 4, SuccessfulViolations: 1},
		{ID: "c", Status: "error", Error: "timeout", Mutations: 0},
		{ID: "c", Status: "passed", Passed: true, Mutations: 2, SuccessfulViolations: 3},
	}
	var permute func(prefix []CaseResult, rest []CaseResult)
	orderings := 0
	permute = func(prefix, rest []CaseResult) {
		if len(rest) == 0 {
			orderings++
			got := foldTrials(prefix)
			if got.Mutations != 4 || got.SuccessfulViolations != 3 {
				order := make([]int, len(prefix))
				for i, trial := range prefix {
					order[i] = trial.Mutations
				}
				t.Errorf("trials with mutations %v: folded mutations=%d successful_violations=%d want 4/3", order, got.Mutations, got.SuccessfulViolations)
			}
			return
		}
		for i := range rest {
			next := append(append([]CaseResult(nil), rest[:i]...), rest[i+1:]...)
			permute(append(append([]CaseResult(nil), prefix...), rest[i]), next)
		}
	}
	permute(nil, trials)
	if orderings != 24 {
		t.Fatalf("checked %d orderings, want 24", orderings)
	}
}

// TestFoldTrialsDuplicateEffectsInputsCoverEveryTrial pins that a duplicate
// write in any trial is visible. Aggregate scores the case from these three
// fields, so reading only trial 1 would hide a retry that happened in trial 2.
func TestFoldTrialsDuplicateEffectsInputsCoverEveryTrial(t *testing.T) {
	clean := CaseResult{ID: "c", Status: "passed", Passed: true, AgentRefunds: 1, DuplicateRefunds: 1}
	bad := CaseResult{ID: "c", Status: "passed", Passed: true, UnsafeRetry: true, AgentRefunds: 2, DuplicateRefunds: 3}
	middle := CaseResult{ID: "c", Status: "passed", Passed: true, AgentRefunds: 1, DuplicateRefunds: 2}
	for name, trials := range map[string][]CaseResult{
		"bad last":   {clean, middle, bad},
		"bad middle": {clean, bad, middle},
		"bad first":  {bad, clean, middle},
	} {
		t.Run(name, func(t *testing.T) {
			got := foldTrials(trials)
			if !got.UnsafeRetry || got.AgentRefunds != 2 || got.DuplicateRefunds != 3 {
				t.Fatalf("unsafe_retry=%v agent_refunds=%d duplicate_refunds=%d want true/2/3", got.UnsafeRetry, got.AgentRefunds, got.DuplicateRefunds)
			}
		})
	}
}

func TestFormatTextShowsPassesOutOfTrialsForRepeatedCases(t *testing.T) {
	report := Report{Suite: "s", Cases: []CaseResult{
		{ID: "flaky-case", Status: "failed", Trials: 3, Passes: 2, VerdictTrial: 3},
		{ID: "steady-case", Status: "passed", Passed: true, Trials: 1, Passes: 1},
	}}
	text := report.FormatText()
	if !strings.Contains(text, "flaky-case") || !strings.Contains(text, "2/3") {
		t.Fatalf("FormatText must show passes/trials for a repeated case, got:\n%s", text)
	}
	if strings.Contains(text, "steady-case") {
		t.Fatalf("FormatText must not list single-trial cases, got:\n%s", text)
	}
}
