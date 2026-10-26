package bench

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseStandardShape(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	raw := []byte(`
version: 1
suite: standard
cases:
  - id: billing-duplicate
    category: reasoning
    world: billing
    scenario: duplicate-charge
    assertions: assertions/duplicate-charge.yaml
    dimensions: [task]
`)
	suite, err := Parse(raw, root)
	if err != nil {
		t.Fatal(err)
	}
	if suite.Name != "standard" || len(suite.Cases) != 1 || suite.Cases[0].Assertions != "assertions/duplicate-charge.yaml" {
		t.Fatalf("%+v", suite)
	}
	if suite.Digest() == "" || suite.Digest() != suite.Digest() {
		t.Fatal("digest unstable")
	}
}

func TestParseRejects(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	cases := map[string]string{
		"bad version":     "version: 2\nsuite: x\ncases:\n  - id: a\n    category: reasoning\n    world: billing\n    scenario: duplicate-charge\n    dimensions: [task]\n",
		"unknown field":   "version: 1\nsuite: x\nextra: 1\ncases:\n  - id: a\n    category: reasoning\n    world: billing\n    scenario: duplicate-charge\n    dimensions: [task]\n",
		"bad category":    "version: 1\nsuite: x\ncases:\n  - id: a\n    category: fluff\n    world: billing\n    scenario: duplicate-charge\n    dimensions: [task]\n",
		"escape path":     "version: 1\nsuite: x\ncases:\n  - id: a\n    category: reasoning\n    world: billing\n    scenario: duplicate-charge\n    assertions: ../go.mod\n    dimensions: [task]\n",
		"missing file":    "version: 1\nsuite: x\ncases:\n  - id: a\n    category: reasoning\n    world: billing\n    scenario: duplicate-charge\n    assertions: assertions/nope.yaml\n    dimensions: [task]\n",
		"unsafe recovery": "version: 1\nsuite: x\ncases:\n  - id: a\n    category: safety\n    world: billing\n    scenario: duplicate-charge\n    recovery: unsafe\n    dimensions: [task]\n",
		"no dimensions":   "version: 1\nsuite: x\ncases:\n  - id: a\n    category: reasoning\n    world: billing\n    scenario: duplicate-charge\n",
		"duplicate id":    "version: 1\nsuite: x\ncases:\n  - id: a\n    category: reasoning\n    world: billing\n    scenario: duplicate-charge\n    dimensions: [task]\n  - id: a\n    category: reasoning\n    world: billing\n    scenario: duplicate-charge\n    dimensions: [task]\n",
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw), root); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseStandardSuiteFile(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	raw, err := os.ReadFile(filepath.Join(root, "bench", "standard.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	suite, err := Parse(raw, root)
	if err != nil {
		t.Fatal(err)
	}
	if suite.Name != "standard" || len(suite.Cases) != 21 {
		t.Fatalf("suite=%s len=%d", suite.Name, len(suite.Cases))
	}
	seen := map[string]bool{}
	for _, c := range suite.Cases {
		seen[c.Category] = true
	}
	for _, cat := range []string{"reliability", "reasoning", "safety", "security", "recovery", "long_horizon", "distributed", "counterfactual"} {
		if !seen[cat] {
			t.Fatalf("missing category %s", cat)
		}
	}
}

func TestParseRequiresExamplesRoot(t *testing.T) {
	if _, err := Parse([]byte("version: 1\nsuite: x\ncases: []\n"), filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(err.Error(), "examples root") {
		t.Fatalf("err=%v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "not-dir"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestIntegerCrashAtKeepsItsMeaning(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	raw := []byte(`
version: 1
suite: crash-at
cases:
  - id: integer-crash
    category: distributed
    world: billing
    scenario: duplicate-charge
    mode: distributed
    crash_at: 3
    assertions: assertions/duplicate-charge.yaml
    dimensions: [task, recovery, duplicate_effects]
`)
	suite, err := Parse(raw, root)
	if err != nil {
		t.Fatal(err)
	}
	if got := suite.Cases[0].CrashPoint; got != "" {
		t.Fatalf("CrashPoint=%q; a bare crash_at must leave it empty, meaning after-model-request", got)
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
	if result.Status != "passed" {
		t.Fatalf("status=%s err=%s failed=%v", result.Status, result.Error, result.FailedChecks)
	}
	d := result.Distributed
	if d == nil {
		t.Fatal("no distributed measurements")
	}
	if d.CrashPoint != "after-model-request" || d.Occurrence != 3 {
		t.Errorf("crash point=%q occurrence=%d want after-model-request 3", d.CrashPoint, d.Occurrence)
	}
	// The same values TestRunStandardSuiteScripted holds the integer cases to.
	if !d.WorkerCrashed || !d.Takeover || !d.ReplayVerified || d.Deliveries != 2 || d.RecoveryFence <= d.CrashedFence {
		t.Errorf("distributed=%+v; want crash, takeover, replay verified, 2 deliveries, recovery fence above the crashed one", *d)
	}
	if d.FencingRejections != 1 {
		t.Errorf("fencing rejections=%d want 1", d.FencingRejections)
	}
	if len(d.Violations) != 0 {
		t.Errorf("violations=%v", d.Violations)
	}
}

func TestCrashPointOutsideDistributedModeIsRefused(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	for _, field := range []string{"crash_point: after-tool-commit", "crash_operation: createRefund"} {
		raw := "version: 1\nsuite: x\ncases:\n  - id: a\n    category: reasoning\n    world: billing\n    scenario: duplicate-charge\n    " + field + "\n    dimensions: [task]\n"
		_, err := Parse([]byte(raw), root)
		if err == nil {
			t.Errorf("%s: accepted on a local case", field)
			continue
		}
		name := strings.SplitN(field, ":", 2)[0]
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%s: error %q does not name the field", field, err)
		}
	}
}

func TestUnknownCrashPointIsRefused(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	raw := "version: 1\nsuite: x\ncases:\n  - id: a\n    category: distributed\n    world: billing\n    scenario: duplicate-charge\n    mode: distributed\n    crash_point: mid-flight\n    crash_at: 1\n    dimensions: [task]\n"
	_, err := Parse([]byte(raw), root)
	if err == nil {
		t.Fatal("accepted crash_point mid-flight")
	}
	if !strings.Contains(err.Error(), "crash_point") || !strings.Contains(err.Error(), "mid-flight") {
		t.Errorf("error %q does not name the field and the value", err)
	}
}

func TestCrashOperationNeedsACrashPoint(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	raw := "version: 1\nsuite: x\ncases:\n  - id: a\n    category: distributed\n    world: billing\n    scenario: duplicate-charge\n    mode: distributed\n    crash_operation: createRefund\n    crash_at: 1\n    dimensions: [task]\n"
	_, err := Parse([]byte(raw), root)
	if err == nil {
		t.Fatal("accepted crash_operation without crash_point")
	}
	if !strings.Contains(err.Error(), "crash_operation") {
		t.Errorf("error %q does not name the field", err)
	}
}
