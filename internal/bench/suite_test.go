package bench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func TestCrashOperationNeedsAToolCrashPoint(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	// Only the tool commit points carry an operation id, so crash_operation on
	// any other point could never match and the case would fail at run time.
	for _, point := range []string{"before-model-request", "after-model-request", "after-model-response", "before-completion", "after-completion"} {
		raw := "version: 1\nsuite: x\ncases:\n  - id: a\n    category: distributed\n    world: billing\n    scenario: duplicate-charge\n    mode: distributed\n    crash_point: " + point + "\n    crash_operation: createRefund\n    crash_at: 1\n    dimensions: [task]\n"
		_, err := Parse([]byte(raw), root)
		if err == nil {
			t.Errorf("%s: accepted crash_operation on a point that carries no operation", point)
			continue
		}
		if !strings.Contains(err.Error(), "crash_operation") || !strings.Contains(err.Error(), point) {
			t.Errorf("%s: error %q does not name the field and the point", point, err)
		}
	}
	for _, point := range []string{"before-tool-commit", "after-tool-commit"} {
		raw := "version: 1\nsuite: x\ncases:\n  - id: a\n    category: distributed\n    world: billing\n    scenario: duplicate-charge\n    mode: distributed\n    crash_point: " + point + "\n    crash_operation: createRefund\n    crash_at: 1\n    dimensions: [task]\n"
		if _, err := Parse([]byte(raw), root); err != nil {
			t.Errorf("%s: refused a valid operation: %v", point, err)
		}
	}
}

func TestAfterCompletionHasOnlyOneOccurrence(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	raw := "version: 1\nsuite: x\ncases:\n  - id: a\n    category: distributed\n    world: billing\n    scenario: duplicate-charge\n    mode: distributed\n    crash_point: after-completion\n    crash_at: 2\n    dimensions: [task]\n"
	_, err := Parse([]byte(raw), root)
	if err == nil {
		t.Fatal("accepted crash_at 2 on after-completion, which is reached once per run")
	}
	if !strings.Contains(err.Error(), "crash_at") || !strings.Contains(err.Error(), "after-completion") {
		t.Errorf("error %q does not name the field and the point", err)
	}
	ok := strings.Replace(raw, "crash_at: 2", "crash_at: 1", 1)
	if _, err := Parse([]byte(ok), root); err != nil {
		t.Errorf("refused crash_at 1 on after-completion: %v", err)
	}
}

// legacyCase is Case as it was before the crash point fields existed. A suite
// digest is recorded in reports, so a suite that does not use the new fields has
// to keep the digest it always had.
type legacyCase struct {
	ID            string
	Category      string
	World         string
	Scenario      string
	Chaos         string
	Auth          string
	Assertions    string
	Recovery      string
	Fault         string
	Steps         int
	Resume        bool
	ResumeAt      int
	Dimensions    []string
	Expect        string
	Mode          string
	CrashAt       int
	Interventions string
}

func TestSuiteDigestIgnoresUnsetCrashFields(t *testing.T) {
	c := Case{ID: "a", Category: "distributed", World: "billing", Scenario: "duplicate-charge",
		Recovery: "safe", Steps: 20, Dimensions: []string{"task"}, Expect: ExpectPass, Mode: ModeDistributed, CrashAt: 3}
	legacy := legacyCase{ID: c.ID, Category: c.Category, World: c.World, Scenario: c.Scenario,
		Recovery: c.Recovery, Steps: c.Steps, Dimensions: c.Dimensions, Expect: c.Expect, Mode: c.Mode, CrashAt: c.CrashAt}
	body := map[string]any{"version": 1, "suite": "s", "cases": []legacyCase{legacy}}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])
	if got := (Suite{Version: 1, Name: "s", Cases: []Case{c}}).Digest(); got != want {
		t.Errorf("digest %s differs from the one computed without the crash fields, %s", got, want)
	}
	c.CrashPoint = "after-tool-commit"
	if got := (Suite{Version: 1, Name: "s", Cases: []Case{c}}).Digest(); got == want {
		t.Error("digest ignores a case that sets crash_point")
	}
}
