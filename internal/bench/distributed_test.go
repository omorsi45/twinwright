package bench

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// The distributed and counterfactual categories exist because the runtime's
// headline features were unmeasured by its own benchmark: nothing in the suite
// crashed a worker, took a run over, or explained a failure. A benchmark that
// cannot see the feature the project leads with is not measuring the project.

func TestDistributedCaseSurvivesWorkerCrashAndFencesTheZombie(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	raw := []byte(`
version: 1
suite: distributed-smoke
cases:
  - id: distributed-crash-midrun
    category: distributed
    world: billing
    scenario: duplicate-charge
    mode: distributed
    crash_at: 2
    assertions: assertions/duplicate-charge.yaml
    dimensions: [task, recovery, duplicate_effects]
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
	if len(report.Cases) != 1 {
		t.Fatalf("cases=%d", len(report.Cases))
	}
	result := report.Cases[0]
	if result.Status != "passed" || !result.Passed {
		t.Fatalf("status=%s failed=%v err=%s", result.Status, result.FailedChecks, result.Error)
	}

	// The four distributed properties the brief names, each measured rather
	// than asserted in prose.
	if result.Distributed == nil {
		t.Fatal("a distributed case reported no distributed measurements")
	}
	if !result.Distributed.WorkerCrashed {
		t.Error("no worker crash was recorded")
	}
	if !result.Distributed.Takeover {
		t.Error("recovery was not recorded as a takeover")
	}
	if result.Distributed.RecoveryFence <= result.Distributed.CrashedFence {
		t.Errorf("recovery fence %d did not exceed the crashed worker's %d",
			result.Distributed.RecoveryFence, result.Distributed.CrashedFence)
	}
	if result.Distributed.Deliveries < 2 {
		t.Errorf("deliveries=%d; a crash and takeover is at least two", result.Distributed.Deliveries)
	}
	if result.Distributed.FencingRejections != 1 {
		t.Errorf("fencing rejections=%d want 1", result.Distributed.FencingRejections)
	}

	// Duplicate delivery must not mean a duplicate effect.
	if result.DuplicateRefunds != 1 {
		t.Errorf("refunds=%d want exactly 1 despite %d deliveries",
			result.DuplicateRefunds, result.Distributed.Deliveries)
	}
	if result.UnsafeRetry {
		t.Error("recovery was recorded as an unsafe retry")
	}
	if !result.Distributed.ReplayVerified {
		t.Error("the recovered run does not verify under replay")
	}
}

func TestCounterfactualCaseExplainsAnUnsafeRecovery(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	raw := []byte(`
version: 1
suite: counterfactual-smoke
cases:
  - id: counterfactual-unsafe-retry
    category: counterfactual
    world: billing
    scenario: ambiguous-commit
    mode: counterfactual
    recovery: unsafe
    chaos: chaos/ambiguous-commit.yaml
    assertions: assertions/ambiguous-commit.yaml
    interventions: counterfactual/ambiguous-commit.yaml
    dimensions: [explanation]
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
	result := report.Cases[0]
	if result.Error != "" {
		t.Fatalf("error=%s", result.Error)
	}

	// The parent must have failed: a counterfactual explains a failure, so a
	// passing parent would mean the case is measuring nothing.
	if result.Counterfactual == nil {
		t.Fatal("a counterfactual case reported no analysis")
	}
	if result.Counterfactual.ParentPassed {
		t.Fatal("the counterfactual parent passed; there is no failure to explain")
	}
	if len(result.Counterfactual.ParentFailed) == 0 {
		t.Error("the parent's failed checks were not recorded")
	}
	if result.Counterfactual.Candidates == 0 {
		t.Error("no interventions were evaluated")
	}
	// An explanation is an intervention that flips the outcome. Finding one is
	// the measurement; the case passes when the analysis explains the failure.
	if !result.Counterfactual.Explained {
		t.Errorf("no intervention explained the failure (candidates=%d)", result.Counterfactual.Candidates)
	}
	if result.Counterfactual.Explanation == "" {
		t.Error("the explaining intervention was not named")
	}
	if result.Status != "passed" || !result.Passed {
		t.Fatalf("status=%s passed=%v", result.Status, result.Passed)
	}
}

// A crash point past the end of the fixture injects no crash at all. Reporting
// that as a successful recovery would be the worst kind of false positive: a
// green distributed case that never tested anything distributed.
func TestDistributedCaseRefusesToPassWhenNoCrashHappened(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	raw := []byte(`
version: 1
suite: distributed-no-crash
cases:
  - id: distributed-crash-too-late
    category: distributed
    world: billing
    scenario: duplicate-charge
    mode: distributed
    crash_at: 99
    assertions: assertions/duplicate-charge.yaml
    dimensions: [task, recovery]
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
	result := report.Cases[0]
	if result.Status != "error" || result.Passed {
		t.Fatalf("status=%s passed=%v; a case with no injected crash must not pass", result.Status, result.Passed)
	}
	if !strings.Contains(result.Error, "completed before model turn 99") {
		t.Fatalf("error=%q does not say the crash never happened", result.Error)
	}
	if result.Distributed != nil && result.Distributed.WorkerCrashed {
		t.Error("a crash was reported when none was injected")
	}
}

// The suite grammar has to refuse a case whose fields contradict its mode,
// because a silently ignored crash_at would report a distributed measurement
// that never happened.
func TestSuiteRejectsModeFieldMismatches(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	base := `
version: 1
suite: invalid
cases:
  - id: case
    category: %s
    world: billing
    scenario: duplicate-charge
    %s
    dimensions: [task]
`
	for name, spec := range map[string]struct {
		category string
		fields   string
		wantErr  string
	}{
		"unknown mode":                   {"reliability", "mode: sideways", "unknown mode"},
		"crash_at without distributed":   {"reliability", "crash_at: 2", "crash_at requires mode distributed"},
		"distributed without crash_at":   {"distributed", "mode: distributed", "mode distributed requires crash_at"},
		"interventions without mode":     {"reliability", "interventions: counterfactual/ambiguous-commit.yaml", "interventions requires mode counterfactual"},
		"counterfactual without file":    {"counterfactual", "mode: counterfactual", "mode counterfactual requires interventions"},
		"zero crash_at":                  {"distributed", "mode: distributed\n    crash_at: 0", "crash_at must be positive"},
		"missing intervention file":      {"counterfactual", "mode: counterfactual\n    interventions: counterfactual/nope.yaml", "missing file"},
		"distributed needs a real world": {"distributed", "mode: distributed\n    crash_at: 2\n    resume: true\n    resume_at: 2", "resume cannot be combined with mode distributed"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(strings.Replace(base, "%s", spec.category, 1)), root)
			_ = err // the category alone is valid; the field combination is what must fail
			doc := strings.Replace(strings.Replace(base, "%s", spec.category, 1), "%s", spec.fields, 1)
			_, err = Parse([]byte(doc), root)
			if err == nil {
				t.Fatalf("accepted:\n%s", doc)
			}
			if !strings.Contains(err.Error(), spec.wantErr) {
				t.Fatalf("err=%q want it to mention %q", err, spec.wantErr)
			}
		})
	}
}
