package bench

import (
	"path/filepath"
	"strings"
	"testing"
)

// The summary is the first thing a reader sees, so it must not conflate three
// different things: an agent that behaved badly, a case that could not run at
// all, and a case whose whole purpose is to fail. Each test below fixes one of
// those conflations.

// A case that errored produced no run and evaluated no assertions, so it
// measured nothing. Scoring it as a compliance failure reports a defect in the
// agent where the truth is that the harness stopped early.
func TestErroredCaseIsNotScoredAsAComplianceFailure(t *testing.T) {
	report := Report{
		Suite: "t",
		Cases: []CaseResult{
			{ID: "ok", Category: "reliability", Status: "passed", Passed: true, Dimensions: []string{"task"}},
			{ID: "broke", Category: "reliability", Status: "error", Error: "fixture cannot continue", Dimensions: []string{"task", "safety"}},
		},
	}
	report.Aggregate()

	if report.Summary.TaskSuccess != 100 {
		t.Fatalf("task success = %.1f, want 100: an errored case must leave the denominator", report.Summary.TaskSuccess)
	}
	if report.Summary.Errors != 1 {
		t.Fatalf("errors = %d, want 1: an errored case must be reported, not hidden", report.Summary.Errors)
	}
	// Excluding it from the rate is only honest if the reader is told. A rate of
	// 100% with a silent error is worse than the original bug.
	text := report.FormatText()
	if !strings.Contains(text, "Errored Cases") {
		t.Fatalf("FormatText must surface errored cases, got:\n%s", text)
	}
}

// A negative control is a case whose fixture is deliberately wrong: the unsafe
// ambiguous-commit fixture, the over-privileged injection principal. It is
// supposed to fail, and the benchmark succeeds when it catches it. Averaging it
// into Safety Compliance reports the harness's success as the agent's failure.
func TestNegativeControlsAreScoredAsDetectionsNotFailures(t *testing.T) {
	report := Report{
		Suite: "t",
		Cases: []CaseResult{
			{ID: "real", Category: "safety", Status: "passed", Passed: true, Dimensions: []string{"safety"}},
			{ID: "control", Category: "safety", Status: "failed", Passed: false, Expect: ExpectFail, Dimensions: []string{"safety"}},
		},
	}
	report.Aggregate()

	if report.Summary.SafetyCompliance != 100 {
		t.Fatalf("safety compliance = %.1f, want 100: a deliberate control is not an agent failure", report.Summary.SafetyCompliance)
	}
	if report.Summary.ControlsTotal != 1 || report.Summary.ControlsDetected != 1 {
		t.Fatalf("controls = %d/%d, want 1/1", report.Summary.ControlsDetected, report.Summary.ControlsTotal)
	}
	text := report.FormatText()
	if !strings.Contains(text, "Negative Controls") {
		t.Fatalf("FormatText must report negative controls, got:\n%s", text)
	}
}

// The failure mode that matters for a control is the silent one: the known-bad
// fixture stops being caught and the suite goes green. That must show up as an
// undetected control rather than as a pass.
func TestNegativeControlThatPassesIsReportedAsUndetected(t *testing.T) {
	report := Report{
		Suite: "t",
		Cases: []CaseResult{
			{ID: "control", Category: "safety", Status: "passed", Passed: true, Expect: ExpectFail, Dimensions: []string{"safety"}},
		},
	}
	report.Aggregate()

	if report.Summary.ControlsTotal != 1 {
		t.Fatalf("controls total = %d, want 1", report.Summary.ControlsTotal)
	}
	if report.Summary.ControlsDetected != 0 {
		t.Fatalf("controls detected = %d, want 0: a control that passed was not caught", report.Summary.ControlsDetected)
	}
	if report.Cases[0].AsExpected {
		t.Fatal("a control that passed did not behave as expected")
	}
}

// A control that deliberately double-refunds must not raise the duplicate-effect
// rate either: that rate exists to say how often the agent duplicated a
// committed effect it should not have.
func TestNegativeControlDoesNotRaiseTheDuplicateEffectRate(t *testing.T) {
	report := Report{
		Suite: "t",
		Cases: []CaseResult{
			{ID: "real", Category: "recovery", Status: "passed", Passed: true, DuplicateRefunds: 1, Dimensions: []string{"duplicate_effects"}},
			{ID: "control", Category: "safety", Status: "failed", Expect: ExpectFail, UnsafeRetry: true, DuplicateRefunds: 2, Dimensions: []string{"duplicate_effects"}},
		},
	}
	report.Aggregate()

	if report.Summary.DuplicateEffects != 0 {
		t.Fatalf("duplicate effects = %.1f, want 0: the only duplicate came from a deliberate control", report.Summary.DuplicateEffects)
	}
}

// A case that matched its expectation is the normal outcome and should be
// visible on both kinds, so a reader can tell "failed, as intended" from
// "failed, unexpectedly" without knowing the fixture names.
func TestAsExpectedIsSetForBothKinds(t *testing.T) {
	report := Report{
		Suite: "t",
		Cases: []CaseResult{
			{ID: "pass-expected", Status: "passed", Passed: true, Category: "reliability", Dimensions: []string{"task"}},
			{ID: "fail-expected", Status: "failed", Category: "safety", Expect: ExpectFail, Dimensions: []string{"safety"}},
			{ID: "fail-unexpected", Status: "failed", Category: "reliability", Dimensions: []string{"task"}},
		},
	}
	report.Aggregate()

	want := map[string]bool{"pass-expected": true, "fail-expected": true, "fail-unexpected": false}
	for _, c := range report.Cases {
		if c.AsExpected != want[c.ID] {
			t.Fatalf("case %s as_expected = %v, want %v", c.ID, c.AsExpected, want[c.ID])
		}
	}
}

// The report can only separate controls from real failures if the suite says
// which cases are controls, so the expectation has to be declarable in the
// document rather than inferred from a case id or a fixture name.
func TestSuiteDeclaresCaseExpectation(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	doc := `version: 1
suite: t
cases:
  - id: ordinary
    category: reliability
    world: billing
    scenario: transient-failure
    dimensions: [task]
  - id: control
    category: safety
    world: billing
    scenario: ambiguous-commit
    recovery: unsafe
    expect: fail
    dimensions: [safety]
`
	suite, err := Parse([]byte(doc), root)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if suite.Cases[0].Expect != ExpectPass {
		t.Fatalf("default expect = %q, want %q", suite.Cases[0].Expect, ExpectPass)
	}
	if suite.Cases[1].Expect != ExpectFail {
		t.Fatalf("declared expect = %q, want %q", suite.Cases[1].Expect, ExpectFail)
	}
}

// A misspelled expectation must be refused rather than defaulted to pass: a
// control silently demoted to an ordinary case is exactly the conflation this
// milestone removes, and it would be invisible in the report.
func TestUnknownExpectationIsRefused(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	doc := `version: 1
suite: t
cases:
  - id: control
    category: safety
    world: billing
    scenario: ambiguous-commit
    recovery: unsafe
    expect: failure
    dimensions: [safety]
`
	if _, err := Parse([]byte(doc), root); err == nil {
		t.Fatal("expected a misspelled expect value to be refused")
	} else if !strings.Contains(err.Error(), "expect") {
		t.Fatalf("error must name the offending field, got: %v", err)
	}
}

// An errored control is not a detection: the fixture never reached the behaviour
// the control exists to catch, so counting it would let a broken harness report
// full detection coverage.
func TestErroredControlIsNotADetection(t *testing.T) {
	report := Report{
		Suite: "t",
		Cases: []CaseResult{
			{ID: "control", Category: "safety", Status: "error", Error: "boom", Expect: ExpectFail, Dimensions: []string{"safety"}},
		},
	}
	report.Aggregate()

	if report.Summary.ControlsDetected != 0 {
		t.Fatalf("controls detected = %d, want 0: an errored control caught nothing", report.Summary.ControlsDetected)
	}
	if report.Summary.Errors != 1 {
		t.Fatalf("errors = %d, want 1", report.Summary.Errors)
	}
}
