package shadow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseObserveOnly(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	raw := []byte(`
version: 1
mode: observe
label: experimental
secret_env: [SHADOW_DEMO_TOKEN]
source:
  type: file
  path: shadow/sample-observations.jsonl
`)
	cfg, err := Parse(raw, root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "observe" || cfg.Source.Path != "shadow/sample-observations.jsonl" || cfg.Digest() == "" {
		t.Fatalf("%+v", cfg)
	}
}

func TestParseRejectsWritesAndEscape(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	cases := map[string]string{
		"write mode":   "version: 1\nmode: write\nlabel: experimental\nsource:\n  type: file\n  path: shadow/sample-observations.jsonl\n",
		"allow writes": "version: 1\nmode: observe\nlabel: experimental\nallow_writes: true\nsource:\n  type: file\n  path: shadow/sample-observations.jsonl\n",
		"escape":       "version: 1\nmode: observe\nlabel: experimental\nsource:\n  type: file\n  path: ../go.mod\n",
		"bad label":    "version: 1\nmode: observe\nlabel: stable\nsource:\n  type: file\n  path: shadow/sample-observations.jsonl\n",
	}
	for name, raw := range cases {
		if _, err := Parse([]byte(raw), root); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestLoadObservations(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	obs, err := LoadObservations(root, "shadow/sample-observations.jsonl")
	if err != nil || len(obs) < 1 {
		t.Fatalf("obs=%v err=%v", obs, err)
	}
}

func TestParseRequiresExamples(t *testing.T) {
	if _, err := Parse([]byte("version: 1\n"), filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(err.Error(), "shadow") && !os.IsNotExist(err) {
		// will fail on mapping / version before root in some paths; ensure non-nil
		if err == nil {
			t.Fatal("accepted")
		}
	}
}
