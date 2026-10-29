package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// The demo stops at the first step whose evidence is missing, so each step's
// evidence check has to be able to fail. These tests give the checks the input
// they exist to refuse. They run the scenarios the steps run, or build the
// smallest document that has the shape a step reads, and need no more of the
// walkthrough than that.

// securityRuns runs prompt-injection-ticket the way step 11 does, once under each
// policy, and returns what the CLI printed.
func securityRuns(t *testing.T) (support, over string) {
	t.Helper()
	examples := filepath.Join("..", "..", "examples")
	dir := t.TempDir()
	manifest := filepath.Join(dir, "company.manifest.json")
	if _, err := demoInvoke("build-world", filepath.Join(examples, "company", "world.yaml"), "--out", manifest); err != nil {
		t.Fatal(err)
	}
	run := func(policy string) string {
		t.Helper()
		out, err := demoInvoke("run", securityScenario,
			"--agent", "scripted", "--seed", "42",
			"--auth", filepath.Join(examples, "security", policy),
			"--manifest", manifest, "--db", filepath.Join(dir, policy+".db"))
		if err != nil {
			t.Fatalf("run under %s: %v", policy, err)
		}
		return out
	}
	return run("support-policy.yaml"), run("overprivileged-policy.yaml")
}

// edited decodes a run's JSON, applies edit to the document and encodes it again.
func edited(t *testing.T, output string, edit func(doc map[string]any)) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(output), &doc); err != nil {
		t.Fatalf("the run printed no JSON: %v: %s", err, output)
	}
	edit(doc)
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// finding returns the security analysis entry of a run document.
func finding(doc map[string]any, name string) map[string]any {
	return doc["security"].(map[string]any)[name].(map[string]any)
}

// Step 11 used to look for the word "injection" in the run's output. The word is
// in the scenario's name, which the output always carries, so the check passed on
// every run and said nothing about whether the injected instruction was refused.
// The evidence is the run's security analysis: the instruction was attempted, the
// policy refused it, and nothing got through.
func TestDemoSecurityStepFailsWithoutAnInjectionFinding(t *testing.T) {
	support, _ := securityRuns(t)

	attempted, refused, err := checkInjectionRefused(support)
	if err != nil {
		t.Fatalf("the support policy's real run was refused as evidence: %v", err)
	}
	if attempted == 0 || refused == 0 {
		t.Errorf("the evidence counts %d attempt(s) and %d refusal(s), want the real counts", attempted, refused)
	}

	tests := []struct {
		name   string
		output string
		want   string
	}{
		{"no security analysis at all", edited(t, support, func(doc map[string]any) { delete(doc, "security") }), "security analysis"},
		{"the attempt was not detected", edited(t, support, func(doc map[string]any) { finding(doc, "attempted_violation")["detected"] = false }), "attempt"},
		{"nothing was refused", edited(t, support, func(doc map[string]any) { finding(doc, "blocked_violation")["detected"] = false }), "refused"},
		{"a refusal without an event", edited(t, support, func(doc map[string]any) { finding(doc, "blocked_violation")["event_ids"] = []any{} }), "refused"},
		{"the instruction got through", edited(t, support, func(doc map[string]any) { finding(doc, "successful_violation")["detected"] = true }), "got through"},
		{"the run failed its evaluation", edited(t, support, func(doc map[string]any) { doc["evaluation"].(map[string]any)["passed"] = false }), "safely"},
		{"output that is not a run", "prompt-injection-ticket: no run", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The scenario's name survives every edit, which is why the old
			// substring could not be the evidence.
			if !strings.Contains(tc.output, "injection") {
				t.Fatalf("the edited output no longer carries the scenario name, so it does not show the old check passing: %s", tc.output)
			}
			_, _, err := checkInjectionRefused(tc.output)
			if err == nil {
				t.Fatalf("the check accepted output with no injection finding: %s", demoExcerpt(tc.output))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not say what is missing (want %q): %v", tc.want, err)
			}
		})
	}
}

// The over-privileged principal is the negative control. It used to pass
// whenever its output was not JSON, because the check only acted on a document
// it could parse that said the evaluation passed.
func TestDemoSecurityControlFailsWhenItCannotBeRead(t *testing.T) {
	_, over := securityRuns(t)

	leaked, err := checkOverPrivilegedCaught(over)
	if err != nil {
		t.Fatalf("the over-privileged policy's real run was refused as evidence: %v", err)
	}
	if leaked == 0 {
		t.Error("the evidence counts no successful violation for the over-privileged principal")
	}

	tests := []struct {
		name   string
		output string
	}{
		{"output that is not JSON", "R-0123456789abcdef01234567 failed: no document"},
		{"the evaluation passed", edited(t, over, func(doc map[string]any) { doc["evaluation"].(map[string]any)["passed"] = true })},
		{"no violation was recorded", edited(t, over, func(doc map[string]any) { finding(doc, "successful_violation")["detected"] = false })},
		{"no security analysis at all", edited(t, over, func(doc map[string]any) { delete(doc, "security") })},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := checkOverPrivilegedCaught(tc.output); err == nil {
				t.Fatalf("the control was accepted: %s", demoExcerpt(tc.output))
			}
		})
	}
}

// Step 4 printed "and 0 spans" for an OTLP document that had resourceSpans and
// nothing in them.
func TestDemoTraceStepFailsWithoutSpans(t *testing.T) {
	span := `{"traceId":"0102","spanId":"0a0b","name":"run"}`
	valid := `{"resourceSpans":[{"resource":{},"scopeSpans":[{"scope":{},"spans":[` + span + `,` + span + `]}]}]}`
	got, err := countOTLPSpans(valid)
	if err != nil || got != 2 {
		t.Fatalf("a document with two spans: got %d, %v", got, err)
	}

	for name, output := range map[string]string{
		"resourceSpans with an empty span list": `{"resourceSpans":[{"resource":{},"scopeSpans":[{"scope":{},"spans":[]}]}]}`,
		"resourceSpans with no scope":           `{"resourceSpans":[{"resource":{},"scopeSpans":[]}]}`,
		"an empty resourceSpans":                `{"resourceSpans":[]}`,
		"no resourceSpans":                      `{}`,
		"not JSON, naming resourceSpans":        `resourceSpans and "spanId" in prose`,
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := countOTLPSpans(output); err == nil {
				t.Fatalf("a trace with no spans was accepted as evidence of %d: %s", got, output)
			}
		})
	}
}

// Step 8 printed "at fence 1 -> 2" as a constant and checked only that the
// recovery fence was higher, so a takeover at fence 3 -> 4 was narrated as 1 -> 2.
func TestDemoCrashStepPrintsTheFencesItRead(t *testing.T) {
	takeover := func(crashed, recovery int64) demoBenchCase {
		return demoBenchCase{ID: "case", Passed: true, Distributed: &demoDistributed{
			WorkerCrashed: true, CrashedFence: crashed, Takeover: true, RecoveryFence: recovery,
			FencingRejections: 1, ReplayVerified: true,
		}}
	}
	tests := []struct {
		name    string
		cases   []demoBenchCase
		want    []string
		notWant []string
	}{
		{"fences 1 to 2", []demoBenchCase{takeover(1, 2)}, []string{"fence 1 -> 2"}, nil},
		{"fences 3 to 4", []demoBenchCase{takeover(3, 4)}, []string{"fence 3 -> 4"}, []string{"1 -> 2"}},
		{"two different pairs", []demoBenchCase{takeover(1, 2), takeover(2, 5)}, []string{"1 -> 2", "2 -> 5"}, nil},
		{"the same pair twice is listed once", []demoBenchCase{takeover(1, 2), takeover(1, 2)}, []string{"fence 1 -> 2,"}, []string{"1 -> 2, 1 -> 2"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evidence, err := summarizeCrashRecovery(demoBench{Cases: tc.cases}, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(evidence, want) {
					t.Errorf("the evidence does not say %q: %s", want, evidence)
				}
			}
			for _, unwanted := range tc.notWant {
				if strings.Contains(evidence, unwanted) {
					t.Errorf("the evidence says %q, which no case showed: %s", unwanted, evidence)
				}
			}
		})
	}

	// The checks that were already there still refuse what they refused.
	equal := takeover(2, 2)
	if _, err := summarizeCrashRecovery(demoBench{Cases: []demoBenchCase{equal}}, ""); err == nil {
		t.Error("a takeover at an equal fence was accepted")
	}
}

// A truncated failure message used to end in U+00E2 U+20AC U+00A6, a UTF-8
// ellipsis decoded as Windows-1252 and encoded again.
func TestDemoExcerptTruncatesWithAnASCIIEllipsis(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := demoExcerpt(long)
	if !strings.HasSuffix(got, "...") {
		t.Errorf("a truncated excerpt ends %q, want ...", got[len(got)-6:])
	}
	if !utf8.ValidString(got) {
		t.Errorf("the excerpt is not valid UTF-8: %q", got)
	}
	for _, r := range got {
		if r > 127 {
			t.Fatalf("the excerpt carries %q (U+%04X); a failing step would print mojibake", r, r)
		}
	}
	if short := demoExcerpt("  fine  "); short != "fine" {
		t.Errorf("a short excerpt is %q, want it trimmed and otherwise unchanged", short)
	}
}
