package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"twinwright/internal/store"
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
	if run["model"] != "fixture-v1" {
		t.Fatalf("saved model=%v", run["model"])
	}
	id := run["id"].(string)
	if err := runCLI([]string{"resume", id, "--agent", "scripted", "--model", "different", "--manifest", manifest, "--db", db}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "model mismatch") {
		t.Fatalf("model change error=%v", err)
	}
	completed := invoke("resume", id, "--agent", "scripted", "--manifest", manifest, "--db", db, "--steps", "10")
	if completed["run"].(map[string]any)["status"] != "completed" || completed["evaluation"].(map[string]any)["passed"] != true {
		t.Fatalf("completed=%v", completed)
	}
	inspected := invoke("inspect", id, "--db", db)
	if len(inspected["events"].([]any)) < 10 {
		t.Fatalf("inspect=%v", inspected)
	}
}

func TestReadManifestRejectsTampering(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	data := []byte(`{"digest":"false-digest","operations":[{"id":"createRefund","method":"POST","path":"/refunds","behavior":"billing.createRefund","required":["charge_id","amount_cents","reason"],"properties":{"charge_id":"string","amount_cents":"integer","reason":"string"}}]}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifest(path); err == nil {
		t.Fatal("tampered manifest accepted")
	}
}

func TestResumeRejectsDifferentManifest(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join("..", "..", "examples", "billing")
	original := filepath.Join(dir, "full.json")
	partial := filepath.Join(dir, "partial.json")
	db := filepath.Join(dir, "world.db")
	if err := runCLI([]string{"build", filepath.Join(root, "openapi.yaml"), "--out", original}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runCLI([]string{"run", "duplicate-charge", "--agent", "scripted", "--manifest", original, "--db", db, "--steps", "1"}, &out); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(original)
	if err != nil {
		t.Fatal(err)
	}
	m.Operations = m.Operations[1:]
	raw, err := json.Marshal(m.Operations)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	m.Digest = hex.EncodeToString(digest[:])
	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(partial, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = readManifest(partial); err != nil {
		t.Fatalf("test manifest invalid: %v", err)
	}
	err = runCLI([]string{"resume", result.Run.ID, "--agent", "scripted", "--manifest", partial, "--db", db}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "manifest mismatch") {
		t.Fatalf("resume error=%v", err)
	}
}

func TestRunRejectsUnknownFaultBeforeCreatingDatabase(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join("..", "..", "examples", "billing")
	manifest := filepath.Join(dir, "manifest.json")
	db := filepath.Join(dir, "world.db")
	if err := runCLI([]string{"build", filepath.Join(root, "openapi.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	err := runCLI([]string{"run", "duplicate-charge", "--agent", "scripted", "--manifest", manifest, "--db", db, "--fault", "listCharge"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unknown fault") {
		t.Fatalf("error=%v", err)
	}
	if _, err = os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("database unexpectedly created: %v", err)
	}
}

func TestFailedRunErrorIncludesRecoverableID(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join("..", "..", "examples", "billing")
	manifest := filepath.Join(dir, "manifest.json")
	db := filepath.Join(dir, "world.db")
	if err := runCLI([]string{"build", filepath.Join(root, "openapi.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	err := runCLI([]string{"run", "duplicate-charge", "--agent", "scripted", "--manifest", manifest, "--db", db, "--fault", "createRefund"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected fixture to fail after refund 503")
	}
	id := regexp.MustCompile(`R-[0-9a-f]{24}`).FindString(err.Error())
	if id == "" {
		t.Fatalf("run ID missing from error: %v", err)
	}
	if err = runCLI([]string{"inspect", id, "--db", db}, &bytes.Buffer{}); err != nil {
		t.Fatalf("saved run is not inspectable: %v", err)
	}
}

func TestCLIReplayCompletedRun(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "billing")
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	db := filepath.Join(dir, "world.db")
	var out bytes.Buffer
	if err := runCLI([]string{"build", filepath.Join(root, "openapi.yaml"), "--out", manifest}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runCLI([]string{"run", "duplicate-charge", "--agent", "scripted", "--manifest", manifest, "--db", db, "--fault", "listCharges"}, &out); err != nil {
		t.Fatal(err)
	}
	var started struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(out.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runCLI([]string{"replay", started.Run.ID, "--manifest", manifest, "--db", db}, &out); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Verified bool `json:"verified"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Verified {
		t.Fatalf("replay=%s", out.String())
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec("UPDATE refunds SET reason='changed'"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err = runCLI([]string{"replay", started.Run.ID, "--manifest", manifest, "--db", db}, &out)
	if err == nil || !strings.Contains(err.Error(), "replay divergence") {
		t.Fatalf("tampered state replay error=%v", err)
	}
	if err = json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Verified {
		t.Fatalf("tampered state accepted: %s", out.String())
	}
}

func TestCLIReplayMissingDatabaseDoesNotCreateFile(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "billing")
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	db := filepath.Join(dir, "missing.db")
	if err := runCLI([]string{"build", filepath.Join(root, "openapi.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := runCLI([]string{"replay", "R-missing", "--manifest", manifest, "--db", db}, &bytes.Buffer{}); err == nil {
		t.Fatal("missing source database accepted")
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("replay created missing database: %v", err)
	}
}

func TestResolveRunModel(t *testing.T) {
	cases := []struct {
		name      string
		agent     string
		requested string
		want      string
	}{
		{name: "default OpenAI model", agent: "openai", want: "gpt-6-sol"},
		{name: "OpenAI override", agent: "openai", requested: "custom-model", want: "custom-model"},
		{name: "scripted fixture", agent: "scripted", want: "fixture-v1"},
		{name: "scripted override", agent: "scripted", requested: "custom-fixture", want: "custom-fixture"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveRunModel(tc.agent, tc.requested); got != tc.want {
				t.Fatalf("resolveRunModel(%q, %q) = %q, want %q", tc.agent, tc.requested, got, tc.want)
			}
		})
	}
}

func TestCompanyScenariosFromCLI(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "company")
	for _, scenario := range []string{"company-incident", "company-routine", "company-no-duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			manifest := filepath.Join(dir, "manifest.json")
			db := filepath.Join(dir, "world.db")
			invoke := func(args ...string) map[string]any {
				t.Helper()
				var out bytes.Buffer
				if err := runCLI(args, &out); err != nil {
					t.Fatal(err)
				}
				var value map[string]any
				if err := json.Unmarshal(out.Bytes(), &value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			invoke("build", filepath.Join(root, "openapi.yaml"), "--bindings", filepath.Join(root, "bindings.yaml"), "--out", manifest)
			runArgs := []string{"run", scenario, "--agent", "scripted", "--manifest", manifest, "--db", db, "--steps", "4"}
			if scenario == "company-incident" {
				runArgs = append(runArgs, "--fault", "createRefund")
			}
			paused := invoke(runArgs...)
			run := paused["run"].(map[string]any)
			if run["status"] != "paused" {
				t.Fatalf("run=%v", run)
			}
			task := run["task"].(string)
			if !strings.Contains(task, "PROJ-ENG") || !strings.Contains(task, "WS-1") {
				t.Fatalf("company resources are not discoverable in task: %s", task)
			}
			id := run["id"].(string)
			completed := invoke("resume", id, "--agent", "scripted", "--manifest", manifest, "--db", db, "--steps", "30")
			if completed["run"].(map[string]any)["status"] != "completed" || completed["evaluation"].(map[string]any)["passed"] != true {
				t.Fatalf("completed=%v", completed)
			}
			inspected := invoke("inspect", id, "--db", db)
			if inspected["evaluation"].(map[string]any)["passed"] != true {
				t.Fatalf("inspect=%v", inspected)
			}
		})
	}
}

func TestBuildWorldRunAndReplay(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "company")
	dir := t.TempDir()
	manifest := filepath.Join(dir, "company.world.manifest.json")
	db := filepath.Join(dir, "company.db")
	invoke := func(args ...string) map[string]any {
		t.Helper()
		var output bytes.Buffer
		if err := runCLI(args, &output); err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(output.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	built := invoke("build-world", filepath.Join(root, "world.yaml"), "--out", manifest)
	if built["digest"] == "" || built["services"] != float64(4) || built["operations"] != float64(18) {
		t.Fatalf("build=%v", built)
	}
	if err := runCLI([]string{"run", "duplicate-charge", "--agent", "scripted", "--manifest", manifest, "--db", db}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("incompatible scenario error=%v", err)
	}
	paused := invoke("run", "company-incident", "--agent", "scripted", "--manifest", manifest, "--db", db, "--fault", "messagePostMessage", "--steps", "4")
	run := paused["run"].(map[string]any)
	if run["status"] != "paused" {
		t.Fatalf("run=%v", run)
	}
	id := run["id"].(string)
	completed := invoke("resume", id, "--agent", "scripted", "--manifest", manifest, "--db", db, "--steps", "30")
	if completed["run"].(map[string]any)["status"] != "completed" || completed["evaluation"].(map[string]any)["passed"] != true {
		t.Fatalf("completed=%v", completed)
	}
	verification := invoke("replay", id, "--manifest", manifest, "--db", db)
	if verification["verified"] != true {
		t.Fatalf("replay=%v", verification)
	}
}

func TestBuildWorldRejectsParentPath(t *testing.T) {
	dir := t.TempDir()
	definition := filepath.Join(dir, "world.yaml")
	content := "version: 1\nname: sample\nseed_profile: company-v1\nservices:\n  - {name: billing, module: billing, openapi: ../billing.yaml, bindings: billing-bindings.yaml}\n"
	if err := os.WriteFile(definition, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	err := runCLI([]string{"build-world", definition, "--out", filepath.Join(dir, "manifest.json")}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "path") {
		t.Fatalf("parent traversal error=%v", err)
	}
}
