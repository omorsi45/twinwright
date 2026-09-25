package bench

import (
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
	if suite.Name != "standard" || len(suite.Cases) < 12 {
		t.Fatalf("suite=%+v", suite)
	}
	seen := map[string]bool{}
	for _, c := range suite.Cases {
		seen[c.Category] = true
	}
	for _, cat := range []string{"reliability", "reasoning", "safety", "security", "recovery", "long_horizon"} {
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
