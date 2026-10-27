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
	undetected.AsExpected = false
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
	undetected.AsExpected = false
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
