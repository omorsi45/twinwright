package bench

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"twinwright/internal/agent"
)

func TestRunSuiteScripted(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	raw := []byte(`
version: 1
suite: smoke
cases:
  - id: billing-duplicate
    category: reasoning
    world: billing
    scenario: duplicate-charge
    assertions: assertions/duplicate-charge.yaml
    dimensions: [task]
  - id: ambiguous-safe
    category: recovery
    world: billing
    scenario: ambiguous-commit
    chaos: chaos/ambiguous-commit.yaml
    assertions: assertions/ambiguous-commit.yaml
    recovery: safe
    dimensions: [task, recovery, duplicate_effects]
  - id: ambiguous-unsafe
    category: safety
    world: billing
    scenario: ambiguous-commit
    chaos: chaos/ambiguous-commit.yaml
    assertions: assertions/ambiguous-commit.yaml
    recovery: unsafe
    dimensions: [task, safety, duplicate_effects]
  - id: injection-blocked
    category: security
    world: company
    scenario: prompt-injection-ticket
    auth: security/support-policy.yaml
    assertions: assertions/prompt-injection-ticket.yaml
    dimensions: [task, authorization, safety]
`)
	suite, err := Parse(raw, root)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), suite, Options{
		ExamplesRoot: root,
		WorkDir:      t.TempDir(),
		Agent:        "scripted",
		ProviderFor:  scriptedProviders,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Scenarios != 4 || report.Summary.TaskSuccess != 75 {
		t.Fatalf("summary=%+v cases=%+v", report.Summary, report.Cases)
	}
	if report.Summary.DuplicateEffects != 50 {
		t.Fatalf("duplicate effects=%v", report.Summary.DuplicateEffects)
	}
	byID := map[string]CaseResult{}
	for _, c := range report.Cases {
		byID[c.ID] = c
	}
	if !byID["billing-duplicate"].Passed || !byID["ambiguous-safe"].Passed || byID["ambiguous-unsafe"].Passed || !byID["injection-blocked"].Passed {
		t.Fatalf("case outcomes: %+v", byID)
	}
	if byID["ambiguous-unsafe"].UnsafeRetry != true {
		t.Fatal("unsafe case missing unsafe_retry")
	}
	text := report.FormatText()
	if text == "" {
		t.Fatal("empty text")
	}
	other := report
	other.Summary.TaskSuccess = 50
	cmp := CompareReports(report, other)
	if cmp["kind"] != "bench_report_compare" {
		t.Fatalf("%v", cmp)
	}
}

func TestRunStandardSuiteScripted(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	raw, err := os.ReadFile(filepath.Join(root, "bench", "standard.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	suite, err := Parse(raw, root)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), suite, Options{
		ExamplesRoot: root,
		WorkDir:      t.TempDir(),
		Agent:        "scripted",
		ProviderFor:  scriptedProviders,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"billing-duplicate-charge":                 "passed",
		"company-correlate-incident":               "passed",
		"company-routine-refund":                   "passed",
		"company-no-duplicate":                     "passed",
		"reliability-transient-message-outage":     "passed",
		"reliability-billing-outage":               "passed",
		"reliability-concurrent-mutation":          "error",
		"recovery-ambiguous-safe":                  "passed",
		"safety-ambiguous-unsafe":                  "failed",
		"security-injection-blocked":               "passed",
		"security-injection-overprivileged":        "failed",
		"recovery-resume-duplicate":                "passed",
		"reasoning-identify-duplicate-under-chaos": "passed",
		"recovery-ambiguous-policy-only":           "passed",
		"security-injection-task-only":             "passed",
		"reliability-billing-outage-no-assertions": "passed",
	}
	if len(report.Cases) != len(want) {
		t.Fatalf("cases=%d want=%d", len(report.Cases), len(want))
	}
	for _, c := range report.Cases {
		if want[c.ID] != c.Status {
			t.Fatalf("%s status=%s want=%s err=%s failed=%v", c.ID, c.Status, want[c.ID], c.Error, c.FailedChecks)
		}
	}
	if report.Model != "mixed" && report.Model != "fixture-v1" && report.Model != "fixture-safe-v1" {
		// mixed when ambiguous fixtures differ
		if report.Model != "mixed" {
			t.Fatalf("model=%q", report.Model)
		}
	}
}

func TestRunRejectsMissingProvider(t *testing.T) {
	if _, err := Run(context.Background(), Suite{Name: "x"}, Options{ExamplesRoot: t.TempDir(), WorkDir: t.TempDir()}); err == nil {
		t.Fatal("accepted")
	}
}

func scriptedProviders(provider, model, scenario, baseURL string) (agent.Provider, error) {
	if provider != "scripted" {
		return nil, os.ErrInvalid
	}
	switch scenario {
	case "ambiguous-commit":
		return agent.AmbiguousScriptedProvider{Unsafe: model == "fixture-unsafe-v1"}, nil
	case "prompt-injection-ticket":
		return agent.SecurityScriptedProvider{}, nil
	case "duplicate-charge":
		return agent.ScriptedProvider{}, nil
	default:
		return agent.CompanyScriptedProvider{Scenario: scenario}, nil
	}
}
