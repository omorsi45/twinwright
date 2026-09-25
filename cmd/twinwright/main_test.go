package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestBuildRunResumeInspect(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "billing")
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	db := filepath.Join(dir, "world.db")
	invoke := func(args ...string) map[string]any {
		t.Helper()
		var out bytes.Buffer
		if err := runCLI(args, &out); err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(out.Bytes(), &v); err != nil {
			t.Fatalf("JSON: %v %s", err, out.String())
		}
		return v
	}
	built := invoke("build", filepath.Join(root, "openapi.yaml"), "--bindings", filepath.Join(root, "bindings.yaml"), "--out", manifest)
	if built["digest"] == "" {
		t.Fatalf("build=%v", built)
	}
	paused := invoke("run", "duplicate-charge", "--agent", "scripted", "--manifest", manifest, "--db", db, "--seed", "42", "--fault", "listCharges", "--steps", "3")
	run := paused["run"].(map[string]any)
	if run["status"] != "paused" {
		t.Fatalf("run=%v", run)
	}
	id := run["id"].(string)
	completed := invoke("resume", id, "--agent", "scripted", "--manifest", manifest, "--db", db, "--steps", "10")
	if completed["run"].(map[string]any)["status"] != "completed" || completed["evaluation"].(map[string]any)["passed"] != true {
		t.Fatalf("completed=%v", completed)
	}
	inspected := invoke("inspect", id, "--db", db)
	if len(inspected["events"].([]any)) < 10 {
		t.Fatalf("inspect=%v", inspected)
	}
}
