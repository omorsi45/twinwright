package gate

import (
	"encoding/json"
	"encoding/xml"
	"strings"
	"testing"

	"twinwright/internal/bench"
)

// Reports are built in memory so each test states exactly the fields it
// compares and nothing else.

func report(version int, cases ...bench.CaseResult) bench.Report {
	r := bench.Report{Suite: "standard", SchemaVersion: version, Cases: cases}
	if version >= 2 {
		r.ContentDigest = "content-1"
	}
	return r
}

func passing(id string) bench.CaseResult {
	return bench.CaseResult{
		ID: id, Status: "passed", Passed: true, AsExpected: true,
		Trials: 1, Passes: 1, Mutations: 1,
		Trajectory: []bench.Step{{Operation: "getCharge", Status: 200}, {Operation: "refundCharge", Status: 200}},
	}
}

func failing(id string, checks ...string) bench.CaseResult {
	c := passing(id)
	c.Status, c.Passed, c.AsExpected, c.Passes, c.FailedChecks = "failed", false, false, 0, checks
	return c
}

func control(id string) bench.CaseResult {
	c := failing(id, "unsafe_retry")
	c.Expect, c.AsExpected = bench.ExpectFail, true
	return c
}

func kinds(findings []Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.Case+":"+f.Kind)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mustCompare(t *testing.T, base, cand bench.Report, opts Options) Result {
	t.Helper()
	result, err := Compare(base, cand, opts)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	return result
}

func TestGatePassesIdenticalReports(t *testing.T) {
	r := report(2, passing("a"), passing("b"), control("c"))
	result := mustCompare(t, r, r, Options{})
	if !result.Passed || len(result.Regressions) != 0 || len(result.Warnings) != 0 || len(result.Improvements) != 0 {
		t.Fatalf("identical reports must gate clean, got %+v", result)
	}
	if result.Compared != 3 || result.Baseline != "standard" || result.Candidate != "standard" {
		t.Fatalf("unexpected header: %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	// Empty lists must serialize as [] so a consumer can iterate them.
	for _, want := range []string{`"regressions":[]`, `"warnings":[]`, `"improvements":[]`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("want %s in %s", want, encoded)
		}
	}
}

func TestGateFlagsNewlyFailingCase(t *testing.T) {
	base := report(2, passing("a"), passing("b"))
	cand := report(2, passing("a"), failing("b", "refund_once", "no_leak"))
	result := mustCompare(t, base, cand, Options{})
	if result.Passed {
		t.Fatal("a newly failing case must not pass the gate")
	}
	if got := kinds(result.Regressions); !equalStrings(got, []string{"b:newly_failing"}) {
		t.Fatalf("regressions = %v", got)
	}
	detail := result.Regressions[0].Detail
	for _, check := range []string{"refund_once", "no_leak"} {
		if !strings.Contains(detail, check) {
			t.Fatalf("detail %q must name failed check %q", detail, check)
		}
	}
}

func TestGateTreatsAnErroredCandidateAsRegressionNotAsMeasurement(t *testing.T) {
	errored := passing("b")
	errored.Status, errored.Passed, errored.Error = "error", false, "provider timeout"
	// Values that would be regressions if an errored case were measured.
	errored.Mutations, errored.SuccessfulViolations, errored.Trajectory = 9, 3, nil
	base := report(2, passing("b"))
	result := mustCompare(t, base, report(2, errored), Options{Trajectory: "fail"})
	if got := kinds(result.Regressions); !equalStrings(got, []string{"b:newly_failing"}) {
		t.Fatalf("regressions = %v", got)
	}
	if !strings.Contains(result.Regressions[0].Detail, "provider timeout") {
		t.Fatalf("detail %q must carry the error", result.Regressions[0].Detail)
	}
}

func TestGateDoesNotFlagACaseThatWasAlreadyFailing(t *testing.T) {
	base := report(2, failing("a", "x"))
	cand := report(2, failing("a", "x", "y"))
	if result := mustCompare(t, base, cand, Options{}); !result.Passed {
		t.Fatalf("a case failing in both reports is not a regression: %+v", result.Regressions)
	}
}

func TestGateReportsAnImprovement(t *testing.T) {
	base := report(2, failing("a", "x"), control("c"))
	undetected := control("c")
	undetected.Status, undetected.Passed, undetected.Passes, undetected.AsExpected = "passed", true, 1, false
	base.Cases[1] = undetected
	cand := report(2, passing("a"), control("c"))
	result := mustCompare(t, base, cand, Options{})
	if !result.Passed {
		t.Fatalf("improvements are not regressions: %+v", result.Regressions)
	}
	if got := kinds(result.Improvements); !equalStrings(got, []string{"a:newly_passing", "c:control_detected"}) {
		t.Fatalf("improvements = %v", got)
	}
}

func TestGateFlagsUndetectedControl(t *testing.T) {
	undetected := control("c")
	undetected.Status, undetected.Passed, undetected.Passes, undetected.AsExpected = "passed", true, 1, false
	errored := control("c")
	errored.Status, errored.AsExpected, errored.Error = "error", false, "boom"
	for name, cand := range map[string]bench.CaseResult{"passes": undetected, "errors": errored} {
		t.Run(name, func(t *testing.T) {
			result := mustCompare(t, report(2, control("c")), report(2, cand), Options{})
			// A control is reported once, as undetected, never also as newly failing.
			if got := kinds(result.Regressions); !equalStrings(got, []string{"c:control_undetected"}) {
				t.Fatalf("regressions = %v", got)
			}
		})
	}
}

func TestGateIgnoresAControlThatWasAlreadyUndetected(t *testing.T) {
	undetected := control("c")
	undetected.Status, undetected.Passed, undetected.Passes, undetected.AsExpected = "passed", true, 1, false
	r := report(2, undetected)
	if result := mustCompare(t, r, r, Options{}); !result.Passed {
		t.Fatalf("a control already undetected in the baseline is not new: %+v", result.Regressions)
	}
}

func TestGateFlagsExtraMutationsAndDuplicateRefunds(t *testing.T) {
	tests := []struct {
		name string
		edit func(*bench.CaseResult)
		want string
	}{
		{"extra mutations", func(c *bench.CaseResult) { c.Mutations = 2 }, "extra_mutations"},
		{"new violation", func(c *bench.CaseResult) { c.SuccessfulViolations = 1 }, "new_violation"},
		{"new unsafe retry", func(c *bench.CaseResult) { c.UnsafeRetry = true }, "new_unsafe_retry"},
		{"more agent refunds", func(c *bench.CaseResult) { c.AgentRefunds = 2 }, "new_duplicate_refund"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cand := passing("a")
			tc.edit(&cand)
			result := mustCompare(t, report(2, passing("a")), report(2, cand), Options{})
			if got := kinds(result.Regressions); !equalStrings(got, []string{"a:" + tc.want}) {
				t.Fatalf("regressions = %v, want a:%s", got, tc.want)
			}
		})
	}
}

func TestGateDoesNotFlagFewerMutationsOrRefunds(t *testing.T) {
	base := passing("a")
	base.Mutations, base.AgentRefunds, base.SuccessfulViolations, base.UnsafeRetry = 3, 2, 1, true
	if result := mustCompare(t, report(2, base), report(2, passing("a")), Options{}); !result.Passed {
		t.Fatalf("reductions are not regressions: %+v", result.Regressions)
	}
}

func TestGateToleranceAllowsSmallPassRateDrop(t *testing.T) {
	trials := func(passes int) bench.CaseResult {
		c := passing("a")
		c.Trials, c.Passes = 10, passes
		if passes < 10 {
			c.Status, c.Passed, c.AsExpected, c.FailedChecks = "failed", false, false, []string{"flaky"}
		}
		return c
	}
	base, cand := report(2, trials(10)), report(2, trials(9))
	if result := mustCompare(t, base, cand, Options{Tolerance: 0.1}); !result.Passed {
		t.Fatalf("a 0.1 drop must pass at tolerance 0.1: %+v", result.Regressions)
	}
	result := mustCompare(t, base, cand, Options{Tolerance: 0.05})
	if got := kinds(result.Regressions); !equalStrings(got, []string{"a:pass_rate_drop"}) {
		t.Fatalf("a 0.1 drop must fail at tolerance 0.05, got %v", got)
	}
	if result := mustCompare(t, base, cand, Options{}); result.Passed {
		t.Fatal("default tolerance is zero, so any drop must fail")
	}
}

func TestGatePassRateTreatsZeroTrialsAsOne(t *testing.T) {
	base := passing("a")
	base.Trials = 0 // a v2 report with no trial count measured one run
	cand := passing("a")
	cand.Trials, cand.Passes = 2, 1
	cand.Status, cand.Passed, cand.AsExpected = "failed", false, false
	result := mustCompare(t, report(2, base), report(2, cand), Options{})
	if got := kinds(result.Regressions); !equalStrings(got, []string{"a:pass_rate_drop"}) {
		t.Fatalf("regressions = %v", got)
	}
}

func TestGateRejectsInvalidOptions(t *testing.T) {
	r := report(2, passing("a"))
	for name, opts := range map[string]Options{
		"negative tolerance": {Tolerance: -0.1},
		"tolerance above 1":  {Tolerance: 1.5},
		"unknown trajectory": {Trajectory: "loud"},
	} {
		if _, err := Compare(r, r, opts); err == nil {
			t.Fatalf("%s: want an error", name)
		}
	}
}

func TestGateTrajectoryModes(t *testing.T) {
	changed := passing("a")
	changed.Trajectory = []bench.Step{{Operation: "getCharge", Status: 200}, {Operation: "refundCharge", Status: 503}}
	base, cand := report(2, passing("a")), report(2, changed)
	tests := []struct {
		mode        string
		regressions int
		warnings    int
	}{
		{"", 0, 1},
		{"warn", 0, 1},
		{"fail", 1, 0},
		{"ignore", 0, 0},
	}
	for _, tc := range tests {
		t.Run("mode "+tc.mode, func(t *testing.T) {
			result := mustCompare(t, base, cand, Options{Trajectory: tc.mode})
			if len(result.Regressions) != tc.regressions || len(result.Warnings) != tc.warnings {
				t.Fatalf("regressions=%v warnings=%v", result.Regressions, result.Warnings)
			}
			all := append(append([]Finding{}, result.Regressions...), result.Warnings...)
			for _, f := range all {
				if f.Kind != "trajectory_changed" || !strings.Contains(f.Detail, "refundCharge") {
					t.Fatalf("unexpected finding %+v", f)
				}
			}
			if result.Passed != (tc.regressions == 0) {
				t.Fatalf("Passed = %v with %d regressions", result.Passed, tc.regressions)
			}
		})
	}
}

func TestGateTrajectoryLengthChangeIsReported(t *testing.T) {
	shorter := passing("a")
	shorter.Trajectory = shorter.Trajectory[:1]
	result := mustCompare(t, report(2, passing("a")), report(2, shorter), Options{})
	if got := kinds(result.Warnings); !equalStrings(got, []string{"a:trajectory_changed"}) {
		t.Fatalf("warnings = %v", got)
	}
}

func TestGateReportsMissingAndNewCases(t *testing.T) {
	base := report(2, passing("a"), passing("gone"))
	cand := report(2, passing("a"), passing("fresh"))
	result := mustCompare(t, base, cand, Options{})
	if got := kinds(result.Regressions); !equalStrings(got, []string{"gone:case_missing"}) {
		t.Fatalf("regressions = %v", got)
	}
	if got := kinds(result.Warnings); !equalStrings(got, []string{"fresh:case_added"}) {
		t.Fatalf("warnings = %v", got)
	}
	if result.Compared != 1 {
		t.Fatalf("Compared = %d, want the one case present in both", result.Compared)
	}
}

func TestGateWarnsWhenSuiteContentChanged(t *testing.T) {
	cand := report(2, passing("a"))
	cand.ContentDigest = "content-2"
	result := mustCompare(t, report(2, passing("a")), cand, Options{})
	if !result.Passed || len(result.Warnings) != 1 || result.Warnings[0].Kind != "suite_changed" {
		t.Fatalf("want one suite_changed warning and a pass, got %+v", result)
	}
}

func TestGateComparesAVersionOneBaseline(t *testing.T) {
	// A v1 report has no trials, mutations, trajectories or content digest, so
	// only status, controls, unsafe retry and agent refunds can be compared.
	v1 := func(cases ...bench.CaseResult) bench.Report {
		for i := range cases {
			cases[i].Trials, cases[i].Passes, cases[i].Mutations, cases[i].Trajectory = 0, 0, 0, nil
		}
		return report(1, cases...)
	}
	drifted := passing("a")
	drifted.Mutations, drifted.SuccessfulViolations = 9, 4
	drifted.Trials, drifted.Passes = 5, 5
	drifted.Trajectory = []bench.Step{{Operation: "somethingElse", Status: 200}}
	result := mustCompare(t, v1(passing("a")), report(2, drifted), Options{Trajectory: "fail"})
	if !result.Passed || len(result.Warnings) != 0 {
		t.Fatalf("v2-only fields must not be compared against v1: %+v", result)
	}

	t.Run("status and controls and refunds still compared", func(t *testing.T) {
		undetected := control("c")
		undetected.Status, undetected.Passed, undetected.AsExpected = "passed", true, false
		retried := passing("r")
		retried.UnsafeRetry, retried.AgentRefunds = true, 2
		base := v1(passing("a"), control("c"), passing("r"))
		cand := report(2, failing("a", "x"), undetected, retried)
		result := mustCompare(t, base, cand, Options{})
		want := []string{"a:newly_failing", "c:control_undetected", "r:new_duplicate_refund", "r:new_unsafe_retry"}
		if got := kinds(result.Regressions); !equalStrings(got, want) {
			t.Fatalf("regressions = %v, want %v", got, want)
		}
	})
}

func TestGateRefusesDifferentSuites(t *testing.T) {
	base := report(2, passing("a"))
	cand := report(2, passing("a"))
	cand.Suite = "other"
	if _, err := Compare(base, cand, Options{}); err == nil || !strings.Contains(err.Error(), "other") {
		t.Fatalf("want a suite mismatch error naming both suites, got %v", err)
	}
}

func TestGateFindingsAreSortedByCaseThenKind(t *testing.T) {
	a := passing("a")
	a.Mutations, a.UnsafeRetry = 5, true
	z := failing("z", "x")
	result := mustCompare(t, report(2, passing("z"), passing("a"), passing("m")), report(2, z, a), Options{})
	want := []string{"a:extra_mutations", "a:new_unsafe_retry", "m:case_missing", "z:newly_failing"}
	if got := kinds(result.Regressions); !equalStrings(got, want) {
		t.Fatalf("regressions = %v, want %v", got, want)
	}
}

func TestMarkdownSummarizesFindings(t *testing.T) {
	base := report(2, passing("a"), passing("gone"))
	cand := report(2, failing("a", "refund_once"), passing("fresh"))
	md := mustCompare(t, base, cand, Options{}).Markdown()
	for _, want := range []string{"FAILED", "newly_failing", "case_missing", "case_added", "refund_once", "standard"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}
	clean := report(2, passing("a"))
	if md := mustCompare(t, clean, clean, Options{}).Markdown(); !strings.Contains(md, "PASSED") || strings.Contains(md, "FAILED") {
		t.Fatalf("clean markdown must say PASSED:\n%s", md)
	}
	piped := report(2, failing("a", "x|y"))
	if md := mustCompare(t, report(2, passing("a")), piped, Options{}).Markdown(); strings.Contains(md, "x|y") {
		t.Fatalf("a pipe in a detail would break the table:\n%s", md)
	}
}

type parsedSuites struct {
	XMLName xml.Name      `xml:"testsuites"`
	Suites  []parsedSuite `xml:"testsuite"`
}

type parsedSuite struct {
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Cases    []parsedCase `xml:"testcase"`
}

type parsedCase struct {
	Name    string `xml:"name,attr"`
	Failure *struct {
		Type    string `xml:"type,attr"`
		Message string `xml:"message,attr"`
		Body    string `xml:",chardata"`
	} `xml:"failure"`
}

func TestJUnitHasOneTestcasePerCaseAndFailuresForRegressions(t *testing.T) {
	base := report(2, passing("a"), passing("b"), passing("c"))
	cand := report(2, passing("a"), failing("b", "refund_once"), passing("d"))
	data, err := mustCompare(t, base, cand, Options{}).JUnit()
	if err != nil {
		t.Fatal(err)
	}
	var parsed parsedSuites
	if err := xml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("not well-formed XML: %v\n%s", err, data)
	}
	if len(parsed.Suites) != 1 || parsed.Suites[0].Name != "standard" {
		t.Fatalf("want one suite named standard: %+v", parsed)
	}
	suite := parsed.Suites[0]
	byName := map[string]parsedCase{}
	for _, c := range suite.Cases {
		byName[c.Name] = c
	}
	if len(suite.Cases) != 4 || suite.Tests != 4 {
		t.Fatalf("want one testcase for each of a, b, c (missing) and d (added), got %d (tests=%d)", len(suite.Cases), suite.Tests)
	}
	if suite.Failures != 2 {
		t.Fatalf("failures = %d, want 2 (b and c)", suite.Failures)
	}
	for name, kind := range map[string]string{"b": "newly_failing", "c": "case_missing"} {
		f := byName[name].Failure
		if f == nil || f.Type != kind {
			t.Fatalf("%s: want failure of type %s, got %+v", name, kind, f)
		}
	}
	if byName["b"].Failure == nil || !strings.Contains(byName["b"].Failure.Body+byName["b"].Failure.Message, "refund_once") {
		t.Fatalf("b's failure must name the failed check: %+v", byName["b"].Failure)
	}
	if byName["a"].Failure != nil || byName["d"].Failure != nil {
		t.Fatal("a passing and an added case must not carry a failure")
	}
}

func TestJUnitForACleanGateHasNoFailures(t *testing.T) {
	r := report(2, passing("a"))
	data, err := mustCompare(t, r, r, Options{}).JUnit()
	if err != nil {
		t.Fatal(err)
	}
	var parsed parsedSuites
	if err := xml.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Suites[0].Failures != 0 || parsed.Suites[0].Tests != 1 {
		t.Fatalf("unexpected suite: %+v", parsed.Suites[0])
	}
}

func TestGateRefusesAnEmptyBaseline(t *testing.T) {
	// An empty baseline compares nothing, so every candidate would pass. That
	// must be an input error, not a green gate.
	_, err := Compare(report(2), report(2, passing("a")), Options{})
	if err == nil || !strings.Contains(err.Error(), "no cases") {
		t.Fatalf("want an error saying the baseline has no cases, got %v", err)
	}
}

// A baseline whose every case errored compares against nothing, like an empty
// one: each case would be baseline_unmeasured and every candidate would pass.
// It is what a bench run without credentials writes.
func TestGateRefusesABaselineWithNoMeasuredCase(t *testing.T) {
	single := func(id string) bench.CaseResult {
		c := passing(id)
		c.Status, c.Passed, c.AsExpected, c.Passes, c.Error = "error", false, false, 0, "OPENAI_API_KEY is required for openai"
		return c
	}
	tests := []struct {
		name string
		base bench.Report
	}{
		{"single trials", report(2, single("a"), single("b"))},
		{"repeated cases", report(2, repeated("a", "error", 10, 0), repeated("b", "error", 10, 0))},
		{"version 1", report(1, single("a"), single("b"))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compare(tc.base, report(2, passing("a"), passing("b")), Options{})
			if err == nil || !strings.Contains(err.Error(), "reached a verdict") {
				t.Fatalf("want an error saying no baseline case reached a verdict, got %v", err)
			}
		})
	}

	// One measured case is enough: the others are warned about, as before.
	base := report(2, single("a"), passing("b"))
	if _, err := Compare(base, report(2, passing("a"), passing("b")), Options{}); err != nil {
		t.Fatalf("a baseline with one measured case is gateable: %v", err)
	}
	// So is a repeated case that errored on a trial but passed the others.
	partly := report(2, repeated("a", "error", 10, 9))
	if _, err := Compare(partly, report(2, repeated("a", "passed", 10, 10)), Options{}); err != nil {
		t.Fatalf("a baseline case with 9 passes is measured: %v", err)
	}
}

func TestGateRefusesDuplicateCaseIDs(t *testing.T) {
	// Which duplicate wins would decide the verdict, so neither is allowed to.
	dup := report(2, passing("a"), passing("twice"), passing("twice"))
	clean := report(2, passing("a"), passing("twice"))
	for name, pair := range map[string][2]bench.Report{"baseline": {dup, clean}, "candidate": {clean, dup}} {
		t.Run(name, func(t *testing.T) {
			_, err := Compare(pair[0], pair[1], Options{})
			if err == nil || !strings.Contains(err.Error(), `"twice"`) || !strings.Contains(err.Error(), name) {
				t.Fatalf("want an error naming the %s and the duplicate ID, got %v", name, err)
			}
		})
	}
}

// multiControl is a negative control run n times, passes of which went
// undetected (the broken case passed).
func multiControl(passes int) bench.CaseResult {
	c := control("c")
	c.Trials, c.Passes = 10, passes
	if passes == 10 {
		c.Status, c.Passed = "passed", true
	}
	return c
}

func TestGateGatesAMultiTrialControlByDetectionRate(t *testing.T) {
	tests := []struct {
		name       string
		base, cand int
		tolerance  float64
		want       []string
	}{
		{"detection falls", 0, 3, 0, []string{"c:control_undetected"}},
		{"detection rises", 2, 0, 0, nil},
		{"tolerance allows the fall", 0, 3, 0.3, nil},
		{"unchanged", 3, 3, 0, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := mustCompare(t, report(2, multiControl(tc.base)), report(2, multiControl(tc.cand)), Options{Tolerance: tc.tolerance})
			if got := kinds(result.Regressions); !equalStrings(got, tc.want) {
				t.Fatalf("regressions = %v, want %v", got, tc.want)
			}
			for _, f := range append(result.Regressions, result.Improvements...) {
				if f.Kind == "pass_rate_drop" || f.Kind == "pass_rate_improved" {
					t.Fatalf("a control has no pass rate to report, got %+v", f)
				}
			}
		})
	}

	t.Run("detail names both detection rates", func(t *testing.T) {
		result := mustCompare(t, report(2, multiControl(0)), report(2, multiControl(3)), Options{})
		if len(result.Regressions) != 1 {
			t.Fatalf("regressions = %v", kinds(result.Regressions))
		}
		detail := result.Regressions[0].Detail
		if !strings.Contains(detail, "10/10") || !strings.Contains(detail, "7/10") {
			t.Fatalf("detail should name both rates: %s", detail)
		}
	})
}

func TestGateReportsNewUnsafeRetryAndRefundsOfAnErroredCandidate(t *testing.T) {
	errored := func() bench.CaseResult {
		c := passing("a")
		c.Status, c.Passed, c.AsExpected, c.Passes, c.Error = "error", false, false, 0, "boom"
		c.UnsafeRetry, c.AgentRefunds = true, 2
		return c
	}
	tests := []struct {
		name string
		base bench.CaseResult
		want []string
	}{
		{"failing baseline", failing("a", "x"), []string{"a:new_duplicate_refund", "a:new_unsafe_retry"}},
		{"passing baseline", passing("a"), []string{"a:new_duplicate_refund", "a:new_unsafe_retry", "a:newly_failing"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := mustCompare(t, report(2, tc.base), report(2, errored()), Options{})
			if got := kinds(result.Regressions); !equalStrings(got, tc.want) {
				t.Fatalf("regressions = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGateWarnsAndSkipsWhenTheBaselineCaseErrored(t *testing.T) {
	base := passing("a")
	base.Status, base.Passed, base.AsExpected, base.Passes, base.Error = "error", false, false, 0, "boom"
	cand := passing("a")
	cand.UnsafeRetry, cand.AgentRefunds, cand.Mutations = true, 2, 9
	// A second, measured case keeps the baseline gateable: a baseline in which
	// every case errored is refused outright (TestGateRefusesABaselineWithNoMeasuredCase).
	result := mustCompare(t, report(2, base, passing("b")), report(2, cand, passing("b")), Options{})
	if !result.Passed {
		t.Fatalf("an unmeasured baseline cannot be regressed against: %+v", result.Regressions)
	}
	if got := kinds(result.Warnings); !equalStrings(got, []string{"a:baseline_unmeasured"}) {
		t.Fatalf("warnings = %v", got)
	}
}

func TestGateDerivesAControlVerdictFromStatusWhenAsExpectedIsAbsent(t *testing.T) {
	// A version 1 report may not carry as_expected, so it decodes as false for a
	// control that was in fact caught. The verdict is derived the way bench
	// derives it: a control is as expected when it failed.
	caught := control("c")
	caught.AsExpected = false
	missed := control("c")
	missed.Status, missed.Passed, missed.Passes, missed.AsExpected = "passed", true, 1, false

	result := mustCompare(t, report(1, caught), report(2, missed), Options{})
	if got := kinds(result.Regressions); !equalStrings(got, []string{"c:control_undetected"}) {
		t.Fatalf("regressions = %v", got)
	}
	if result := mustCompare(t, report(1, caught), report(2, control("c")), Options{}); !result.Passed {
		t.Fatalf("a caught control stays caught: %+v", result.Regressions)
	}
}

// repeated is a case run trials times, passes of which passed. The status is
// the one bench's fold would give: passed when every trial passed, otherwise
// the given one.
func repeated(id, status string, trials, passes int) bench.CaseResult {
	c := passing(id)
	c.Trials, c.Passes = trials, passes
	c.Status, c.Passed, c.AsExpected = status, status == "passed", status == "passed"
	switch status {
	case "failed":
		c.FailedChecks = []string{"flaky"}
	case "error":
		c.Error = "provider timeout"
	}
	return c
}

func TestGateRatesAnErroredRepeatedCandidate(t *testing.T) {
	base := report(2, repeated("a", "failed", 10, 9))
	tests := []struct {
		name      string
		cand      bench.CaseResult
		wantRegs  []string
		wantWarns []string
	}{
		{"every trial errored", repeated("a", "error", 10, 0), []string{"a:candidate_unmeasured"}, nil},
		{"one pass and nine errored trials", repeated("a", "error", 10, 1), []string{"a:pass_rate_drop"}, []string{"a:candidate_errored"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := mustCompare(t, base, report(2, tc.cand), Options{Tolerance: 0.1})
			if got := kinds(result.Regressions); !equalStrings(got, tc.wantRegs) {
				t.Fatalf("regressions = %v, want %v (passed=%v)", got, tc.wantRegs, result.Passed)
			}
			if got := kinds(result.Warnings); !equalStrings(got, tc.wantWarns) {
				t.Fatalf("warnings = %v, want %v", got, tc.wantWarns)
			}
			if result.Passed {
				t.Fatal("a candidate that lost its passes must not pass the gate")
			}
		})
	}
}

func TestGateRatesARepeatedBaselineWithAnErroredTrial(t *testing.T) {
	// Nine passes and one provider timeout: the nine passes are real, so the
	// case is gateable. Only a baseline with no pass at all is unmeasured.
	base := report(2, repeated("a", "error", 10, 9))
	tests := []struct {
		name      string
		cand      bench.CaseResult
		tolerance float64
		wantRegs  []string
	}{
		{"candidate fails every trial", repeated("a", "failed", 10, 0), 0.1, []string{"a:pass_rate_drop"}},
		{"candidate holds the rate", repeated("a", "failed", 10, 9), 0, nil},
		{"candidate within tolerance", repeated("a", "failed", 10, 8), 0.1, nil},
		{"candidate past tolerance", repeated("a", "failed", 10, 7), 0.1, []string{"a:pass_rate_drop"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := mustCompare(t, base, report(2, tc.cand), Options{Tolerance: tc.tolerance})
			if got := kinds(result.Regressions); !equalStrings(got, tc.wantRegs) {
				t.Fatalf("regressions = %v, want %v", got, tc.wantRegs)
			}
			for _, w := range result.Warnings {
				if w.Kind == KindBaselineUnmeasured {
					t.Fatalf("a baseline with 9 passes is measured, got %+v", w)
				}
			}
		})
	}

	t.Run("a baseline with no pass stays unmeasured", func(t *testing.T) {
		none := report(2, repeated("a", "error", 10, 0), passing("b"))
		result := mustCompare(t, none, report(2, repeated("a", "failed", 10, 0), passing("b")), Options{Tolerance: 0.1})
		if !result.Passed || !equalStrings(kinds(result.Warnings), []string{"a:baseline_unmeasured"}) {
			t.Fatalf("passed=%v warnings=%v", result.Passed, kinds(result.Warnings))
		}
	})
}

// A provider timeout in one trial of ten is the same loss of one pass as one
// failed trial of ten, so the tolerance has to treat them alike.
func TestGateToleratesOneErroredTrialLikeOneFailedTrial(t *testing.T) {
	base := report(2, repeated("a", "passed", 10, 10))
	for _, tolerance := range []float64{0, 0.1} {
		failed := mustCompare(t, base, report(2, repeated("a", "failed", 10, 9)), Options{Tolerance: tolerance})
		errored := mustCompare(t, base, report(2, repeated("a", "error", 10, 9)), Options{Tolerance: tolerance})
		if failed.Passed != errored.Passed || !equalStrings(kinds(failed.Regressions), kinds(errored.Regressions)) {
			t.Errorf("tolerance %g: one failed trial gives %v, one errored trial gives %v",
				tolerance, kinds(failed.Regressions), kinds(errored.Regressions))
		}
	}
	// Within tolerance the error is not a regression, and it is not silent.
	result := mustCompare(t, base, report(2, repeated("a", "error", 10, 9)), Options{Tolerance: 0.1})
	if !result.Passed {
		t.Fatalf("one errored trial in ten is within tolerance 0.1: %+v", result.Regressions)
	}
	if got := kinds(result.Warnings); !equalStrings(got, []string{"a:candidate_errored"}) {
		t.Fatalf("warnings = %v, want the errored trial reported", got)
	}
}

func TestGateRegressesACandidateThatReachedNoVerdictOnAPartlyPassingBaseline(t *testing.T) {
	// The baseline passed 9 of 10, so the candidate has something to lose even
	// when the tolerance is wide enough to allow a drop to zero.
	base := report(2, repeated("a", "failed", 10, 9))
	result := mustCompare(t, base, report(2, repeated("a", "error", 10, 0)), Options{Tolerance: 1})
	if got := kinds(result.Regressions); !equalStrings(got, []string{"a:candidate_unmeasured"}) {
		t.Fatalf("regressions = %v", got)
	}
	if !strings.Contains(result.Regressions[0].Detail, "provider timeout") {
		t.Fatalf("detail %q must carry the error", result.Regressions[0].Detail)
	}
}

func TestGateWarnsWhenACandidateErrorsOnACaseTheBaselineAlreadyFailed(t *testing.T) {
	// There was no pass to lose, so this is not a regression, but a candidate
	// that cannot run a case must not be silent either.
	for name, base := range map[string]bench.CaseResult{
		"single trial": failing("a", "x"),
		"repeated":     repeated("a", "failed", 10, 0),
	} {
		t.Run(name, func(t *testing.T) {
			cand := repeated("a", "error", base.Trials, 0)
			result := mustCompare(t, report(2, base), report(2, cand), Options{})
			if !result.Passed {
				t.Fatalf("regressions = %v", kinds(result.Regressions))
			}
			if got := kinds(result.Warnings); !equalStrings(got, []string{"a:candidate_errored"}) {
				t.Fatalf("warnings = %v", got)
			}
		})
	}
}

// A mixed case (some trials passed, one errored) is still compared on the
// safety counters its trials recorded. An errored trial must not switch off
// extra_mutations or new_violation for the trials that completed, or the
// verdict would depend on whether a provider timed out somewhere in the case.
func TestGateComparesSafetyCountersOfACandidateWithAnErroredTrial(t *testing.T) {
	base := report(2, repeated("a", "passed", 10, 10))
	cand := repeated("a", "error", 10, 9)
	cand.Mutations, cand.SuccessfulViolations = 4, 1
	cand.Trajectory = nil // the first trial errored early, so its trajectory is truncated
	result := mustCompare(t, base, report(2, cand), Options{Tolerance: 0.1, Trajectory: TrajectoryFail})
	want := []string{"a:extra_mutations", "a:new_violation"}
	if got := kinds(result.Regressions); !equalStrings(got, want) {
		t.Fatalf("regressions = %v, want %v", got, want)
	}
}
