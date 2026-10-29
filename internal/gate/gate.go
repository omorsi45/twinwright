// Package gate compares a candidate bench report with an approved baseline and
// decides whether the candidate regressed. It reads two reports and nothing
// else: no database, no manifest, no network. That is what lets CI run it as a
// cheap last step after a bench job, and lets a reviewer reproduce a verdict
// from the two JSON files alone.
package gate

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"twinwright/internal/bench"
)

// Trajectory modes. A changed tool-call sequence is not always a defect (an
// agent may legitimately reorder harmless reads), so how loudly to treat it is
// the caller's choice.
const (
	TrajectoryFail   = "fail"
	TrajectoryWarn   = "warn"
	TrajectoryIgnore = "ignore"
)

// Finding kinds. Regression kinds are exactly these; the gate's exit code is
// derived from them, so renaming one is a change to CI's contract.
const (
	KindCaseMissing         = "case_missing"
	KindNewlyFailing        = "newly_failing"
	KindPassRateDrop        = "pass_rate_drop"
	KindControlUndetected   = "control_undetected"
	KindExtraMutations      = "extra_mutations"
	KindNewViolation        = "new_violation"
	KindNewUnsafeRetry      = "new_unsafe_retry"
	KindNewDuplicateRefund  = "new_duplicate_refund"
	KindCandidateUnmeasured = "candidate_unmeasured"
	KindTrajectoryChanged   = "trajectory_changed"
	KindCaseAdded           = "case_added"
	KindBaselineUnmeasured  = "baseline_unmeasured"
	KindCandidateErrored    = "candidate_errored"
	KindSuiteChanged        = "suite_changed"
	KindNewlyPassing        = "newly_passing"
	KindControlDetected     = "control_detected"
	KindPassRateImprovement = "pass_rate_improved"
)

// rateEpsilon absorbs float64 error in Passes/Trials so that a drop of exactly
// the tolerance (9/10 against 10/10 at tolerance 0.1) is allowed instead of
// failing on the last bit of 1-0.1.
const rateEpsilon = 1e-9

// Options tunes what counts as a regression.
type Options struct {
	// Tolerance is the allowed drop in a case's pass rate, from 0 to 1. The
	// default of 0 treats any drop as a regression.
	Tolerance float64
	// Trajectory is "fail", "warn" (the default when empty) or "ignore".
	Trajectory string
}

// Finding is one difference between the two reports.
type Finding struct {
	Case   string `json:"case"`
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

// Result is the gate's verdict. Passed means there are no regressions;
// warnings and improvements never change it.
type Result struct {
	Passed       bool      `json:"passed"`
	Baseline     string    `json:"baseline_suite"`
	Candidate    string    `json:"candidate_suite"`
	Regressions  []Finding `json:"regressions"`
	Warnings     []Finding `json:"warnings"`
	Improvements []Finding `json:"improvements"`
	Compared     int       `json:"cases_compared"`

	// cases lists every case ID in either report, sorted. It is unexported
	// because the JSON verdict is about differences; JUnit needs the full list
	// to emit a testcase for the cases that did not differ.
	cases []string
}

func (o Options) validate() error {
	if math.IsNaN(o.Tolerance) || o.Tolerance < 0 || o.Tolerance > 1 {
		return fmt.Errorf("tolerance must be between 0 and 1, got %v", o.Tolerance)
	}
	switch o.Trajectory {
	case "", TrajectoryFail, TrajectoryWarn, TrajectoryIgnore:
		return nil
	}
	return fmt.Errorf("trajectory must be fail, warn or ignore, got %q", o.Trajectory)
}

// Compare gates candidate against baseline. It returns an error, instead of a
// verdict, when the two reports measured different suites (a comparison between
// them would be meaningless), when opts is out of range, or when the inputs
// could not support a verdict: a baseline with no cases, or in which every case
// errored, gates nothing, and a repeated case ID in either report would let the
// order of the file decide the result.
//
// Fields that only a version 2 report carries (trials, mutations, violations,
// trajectories) are compared only when both reports are version 2. Comparing a
// zero that means "this report predates the field" against a real measurement
// would flag every case.
func Compare(baseline, candidate bench.Report, opts Options) (Result, error) {
	if err := opts.validate(); err != nil {
		return Result{}, err
	}
	if baseline.Suite != candidate.Suite {
		return Result{}, fmt.Errorf("suite mismatch: baseline is %q, candidate is %q", baseline.Suite, candidate.Suite)
	}
	if len(baseline.Cases) == 0 {
		return Result{}, fmt.Errorf("the baseline has no cases, so there is nothing to gate against")
	}
	// A baseline in which every case errored compares against nothing too: each
	// case would be baseline_unmeasured and every candidate would pass. A bench
	// run without credentials writes exactly that.
	measured := false
	for _, c := range baseline.Cases {
		if !(c.Status == "error" && passedNothing(c)) {
			measured = true
			break
		}
	}
	if !measured {
		return Result{}, fmt.Errorf("the baseline has no case that reached a verdict (every case errored), so there is nothing to gate against")
	}
	base, err := index("baseline", baseline.Cases)
	if err != nil {
		return Result{}, err
	}
	cand, err := index("candidate", candidate.Cases)
	if err != nil {
		return Result{}, err
	}
	if opts.Trajectory == "" {
		opts.Trajectory = TrajectoryWarn
	}
	result := Result{
		Baseline:     baseline.Suite,
		Candidate:    candidate.Suite,
		Regressions:  []Finding{},
		Warnings:     []Finding{},
		Improvements: []Finding{},
	}
	v2 := baseline.SchemaVersion >= 2 && candidate.SchemaVersion >= 2
	if v2 && baseline.ContentDigest != "" && candidate.ContentDigest != "" && baseline.ContentDigest != candidate.ContentDigest {
		result.Warnings = append(result.Warnings, Finding{
			Kind:   KindSuiteChanged,
			Detail: "the fixture content digest differs, so the reports may not have measured the same assertions",
		})
	}

	ids := map[string]bool{}
	for id := range base {
		ids[id] = true
	}
	for id := range cand {
		ids[id] = true
	}
	for id := range ids {
		result.cases = append(result.cases, id)
	}
	sort.Strings(result.cases)

	for _, id := range result.cases {
		b, inBase := base[id]
		c, inCand := cand[id]
		switch {
		case !inCand:
			result.Regressions = append(result.Regressions, Finding{Case: id, Kind: KindCaseMissing, Detail: "the baseline has this case and the candidate does not"})
		case !inBase:
			result.Warnings = append(result.Warnings, Finding{Case: id, Kind: KindCaseAdded, Detail: "the candidate has this case and the baseline does not"})
		default:
			result.Compared++
			compareCase(&result, b, c, v2, opts)
		}
	}

	sortFindings(result.Regressions)
	sortFindings(result.Warnings)
	sortFindings(result.Improvements)
	result.Passed = len(result.Regressions) == 0
	return result, nil
}

// index keys cases by ID. A repeated ID is an error, not a tie to break: either
// copy could be the one the author meant, and the verdict would depend on it. A
// case the gate cannot judge is an error too: bench never writes one, so it came
// from a hand-edited, truncated or foreign report, and the gate's contract for
// input it cannot judge is to refuse it rather than guess.
func index(side string, cases []bench.CaseResult) (map[string]bench.CaseResult, error) {
	out := make(map[string]bench.CaseResult, len(cases))
	for _, c := range cases {
		if err := checkCase(c); err != nil {
			return nil, fmt.Errorf("the %s report: %w", side, err)
		}
		if _, seen := out[c.ID]; seen {
			return nil, fmt.Errorf("the %s report repeats case ID %q", side, c.ID)
		}
		out[c.ID] = c
	}
	return out, nil
}

// checkCase refuses what the comparison would misread. A status other than the
// three bench writes is neither a pass nor a failure, so a passed baseline case
// would regress to it without a finding. An empty ID cannot carry a JUnit
// testcase. A pass count above the trial count is not a measurement; a report
// from before trial counts has zero trials and zero passes, and a trial total
// of zero measured one run.
func checkCase(c bench.CaseResult) error {
	if c.ID == "" {
		return fmt.Errorf("a case has no ID")
	}
	switch c.Status {
	case "passed", "failed", "error":
	default:
		return fmt.Errorf("case %q has status %q, want passed, failed or error", c.ID, c.Status)
	}
	if c.Trials < 0 {
		return fmt.Errorf("case %q has %d trials", c.ID, c.Trials)
	}
	if c.Passes < 0 || c.Passes > max(c.Trials, 1) {
		return fmt.Errorf("case %q has %d passes in %d trials", c.ID, c.Passes, c.Trials)
	}
	return nil
}

func compareCase(r *Result, b, c bench.CaseResult, v2 bool, opts Options) {
	regress := func(kind, format string, args ...any) {
		r.Regressions = append(r.Regressions, Finding{Case: c.ID, Kind: kind, Detail: fmt.Sprintf(format, args...)})
	}
	warn := func(kind, format string, args ...any) {
		r.Warnings = append(r.Warnings, Finding{Case: c.ID, Kind: kind, Detail: fmt.Sprintf(format, args...)})
	}
	improve := func(kind, format string, args ...any) {
		r.Improvements = append(r.Improvements, Finding{Case: c.ID, Kind: kind, Detail: fmt.Sprintf(format, args...)})
	}

	// An errored baseline case that passed nothing reached no verdict, so there
	// is nothing to hold the candidate to. Say so, because a silent skip looks
	// like a pass. A repeated case with some passes and one errored trial is
	// still measured: its passes are real, and discarding them would let a
	// single provider timeout in the baseline switch the case off for good.
	//
	// A control is the exception. Its Passes counts the trials where the broken
	// case went undetected and an errored trial counts as caught (the limit
	// ADR 0030 states), so for a control any error leaves the case unmeasured.
	isControl := b.Expect == bench.ExpectFail && c.Expect == bench.ExpectFail
	unmeasured := func(x bench.CaseResult) bool {
		return x.Status == "error" && (isControl || passedNothing(x))
	}
	bUnmeasured, cUnmeasured := unmeasured(b), unmeasured(c)
	if bUnmeasured {
		warn(KindBaselineUnmeasured, "the baseline errored on this case, so it cannot be gated: %s", outcome(b))
		return
	}
	bErrored, cErrored := b.Status == "error", c.Status == "error"
	// A control is only a control when both reports declare it one. If the
	// declaration changed, the suite changed, and the case is judged as a normal
	// case rather than as an undetected control it never was.
	//
	// A control's Passes counts the trials where the broken case went
	// undetected, so its rate is judged in the opposite direction below.
	controlRated := isControl && v2 && !cErrored && (b.Trials > 1 || c.Trials > 1)
	// An errored trial is not a pass, so a repeated case that errored on some
	// trials is rated on the passes it did record, like one that failed on them.
	// Only a candidate that passed nothing and reached no verdict is handled
	// below instead of being rated.
	rated := !isControl && v2 && !cUnmeasured && (b.Trials > 1 || c.Trials > 1)

	switch {
	case controlRated:
		compareDetection(b, c, opts, regress, improve)
	case isControl:
		bCaught, cCaught := asExpected(b), asExpected(c)
		switch {
		case bCaught && !cCaught:
			regress(KindControlUndetected, "the negative control was caught in the baseline and is no longer: %s", outcome(c))
		case !bCaught && cCaught:
			improve(KindControlDetected, "the negative control is now caught")
		}
	case cUnmeasured && b.Status == "passed":
		regress(KindNewlyFailing, "the baseline passed and the candidate errored: %s", outcome(c))
	case cUnmeasured && b.Passes > 0:
		regress(KindCandidateUnmeasured, "the baseline passed %d of %d trials and the candidate reached no verdict on any: %s",
			b.Passes, trials(b), outcome(c))
	case cUnmeasured:
		// The baseline had no pass to lose, so this is not a regression, but a
		// candidate that cannot run a case must not be silent about it.
		warn(KindCandidateErrored, "the candidate reached no verdict on this case and the baseline had no pass: %s", outcome(c))
	case b.Status == "passed" && c.Status == "failed" && !rated:
		regress(KindNewlyFailing, "the baseline passed and the candidate failed: %s", outcome(c))
	case b.Status != "passed" && c.Status == "passed" && !rated:
		improve(KindNewlyPassing, "the baseline did not pass and the candidate does")
	}
	if cErrored && !cUnmeasured && !isControl {
		warn(KindCandidateErrored, "the candidate errored on at least one trial (%d of %d passed), which counts as not passed: %s",
			c.Passes, trials(c), outcome(c))
	}

	// These two are facts the harness records as it runs, so an erroring
	// candidate that already reissued an unsafe write or refunded twice still
	// did so. Only an unmeasured baseline makes them untrustworthy, handled above.
	if !b.UnsafeRetry && c.UnsafeRetry {
		regress(KindNewUnsafeRetry, "the candidate reissued a write whose outcome was unknown")
	}
	if c.AgentRefunds > b.AgentRefunds {
		regress(KindNewDuplicateRefund, "agent refunds rose from %d to %d", b.AgentRefunds, c.AgentRefunds)
	}
	// A candidate that reached no verdict on any trial stopped early everywhere,
	// so its counters describe wherever it stopped and are not compared against
	// a complete one. A candidate with some completed trials is compared: the
	// counters are the largest over its trials, so a trial that wrote more than
	// the baseline did is not hidden by another trial's error.
	if !v2 || cUnmeasured {
		return
	}
	if c.Mutations > b.Mutations {
		regress(KindExtraMutations, "state mutations rose from %d to %d", b.Mutations, c.Mutations)
	}
	if c.SuccessfulViolations > b.SuccessfulViolations {
		regress(KindNewViolation, "successful violations rose from %d to %d", b.SuccessfulViolations, c.SuccessfulViolations)
	}
	if rated {
		compareRate(b, c, opts, regress, improve)
	}
	// The trajectory is the first trial's, which is truncated when that trial
	// errored, so an errored case on either side has no trajectory to compare.
	if opts.Trajectory != TrajectoryIgnore && !bErrored && !cErrored {
		if detail, changed := trajectoryDiff(b.Trajectory, c.Trajectory); changed {
			if opts.Trajectory == TrajectoryFail {
				regress(KindTrajectoryChanged, "%s", detail)
			} else {
				warn(KindTrajectoryChanged, "%s", detail)
			}
		}
	}
}

// asExpected applies bench's own rule (Report.Aggregate) to a control instead of
// trusting the stored as_expected, which a version 1 report may not carry and
// which then decodes as false for a control that was in fact caught. Only the
// control branch is needed: an errored case is never as expected, and a control
// is as expected when it failed.
func asExpected(c bench.CaseResult) bool {
	if c.Status == "error" {
		return false
	}
	return c.Status == "failed"
}

// compareDetection judges a repeated negative control by how often it was
// caught, (Trials-Passes)/Trials. Regarding Passes as a pass rate would reward
// a control for going undetected more often.
func compareDetection(b, c bench.CaseResult, opts Options, regress, improve func(string, string, ...any)) {
	bRate, cRate := detectionRate(b), detectionRate(c)
	switch {
	case cRate < bRate-opts.Tolerance-rateEpsilon:
		regress(KindControlUndetected, "detection rate fell from %d/%d to %d/%d (tolerance %g)",
			trials(b)-b.Passes, trials(b), trials(c)-c.Passes, trials(c), opts.Tolerance)
	case cRate > bRate+rateEpsilon:
		improve(KindControlDetected, "detection rate rose from %d/%d to %d/%d",
			trials(b)-b.Passes, trials(b), trials(c)-c.Passes, trials(c))
	}
}

func detectionRate(c bench.CaseResult) float64 {
	return float64(trials(c)-c.Passes) / float64(trials(c))
}

// compareRate judges a repeated case by its pass rate, so a flaky agent is
// measured and not failed on one unlucky trial. A report from before trial
// counts existed, or a trial total of zero, measured one run.
func compareRate(b, c bench.CaseResult, opts Options, regress, improve func(string, string, ...any)) {
	bRate, cRate := passRate(b), passRate(c)
	switch {
	case cRate < bRate-opts.Tolerance-rateEpsilon:
		regress(KindPassRateDrop, "pass rate fell from %d/%d to %d/%d (tolerance %g)%s",
			b.Passes, trials(b), c.Passes, trials(c), opts.Tolerance, failedChecks(c))
	case cRate > bRate+rateEpsilon:
		improve(KindPassRateImprovement, "pass rate rose from %d/%d to %d/%d", b.Passes, trials(b), c.Passes, trials(c))
	}
}

func trials(c bench.CaseResult) int {
	if c.Trials < 1 {
		return 1
	}
	return c.Trials
}

// passedNothing is whether a case has no recorded pass to its name. A single
// trial that errored has none whatever Passes says, and a repeated case has none
// when Passes is zero.
func passedNothing(c bench.CaseResult) bool {
	return c.Passes == 0 || trials(c) <= 1
}

func passRate(c bench.CaseResult) float64 {
	return float64(c.Passes) / float64(trials(c))
}

// outcome describes why a candidate case did not pass, naming the failed checks
// or the error so a reader does not have to open the report.
func outcome(c bench.CaseResult) string {
	if c.Status == "error" {
		if c.Error != "" {
			return "error: " + c.Error
		}
		return "error"
	}
	if len(c.FailedChecks) > 0 {
		return "failed checks: " + strings.Join(c.FailedChecks, ", ")
	}
	return "status " + c.Status
}

func failedChecks(c bench.CaseResult) string {
	if len(c.FailedChecks) == 0 {
		return ""
	}
	return "; failed checks: " + strings.Join(c.FailedChecks, ", ")
}

// trajectoryDiff reports where two tool-call sequences first diverge.
func trajectoryDiff(b, c []bench.Step) (string, bool) {
	step := func(s bench.Step) string { return fmt.Sprintf("%s (%d)", s.Operation, s.Status) }
	for i := 0; i < len(b) && i < len(c); i++ {
		if b[i] != c[i] {
			return fmt.Sprintf("tool call %d changed from %s to %s", i+1, step(b[i]), step(c[i])), true
		}
	}
	switch {
	case len(c) > len(b):
		return fmt.Sprintf("the candidate made %d tool calls to the baseline's %d, starting with %s", len(c), len(b), step(c[len(b)])), true
	case len(c) < len(b):
		return fmt.Sprintf("the candidate made %d tool calls to the baseline's %d, omitting %s", len(c), len(b), step(b[len(c)])), true
	}
	return "", false
}

func sortFindings(f []Finding) {
	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Case != f[j].Case {
			return f[i].Case < f[j].Case
		}
		return f[i].Kind < f[j].Kind
	})
}
