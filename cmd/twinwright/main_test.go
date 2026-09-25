package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"twinwright/internal/behavior"
	"twinwright/internal/compiler"
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

func TestChaosCLIInvalidPolicyDoesNotCreateWorld(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join("..", "..", "examples", "billing")
	manifest, db, policy := filepath.Join(dir, "manifest.json"), filepath.Join(dir, "world.db"), filepath.Join(dir, "invalid.yaml")
	if err := runCLI([]string{"build", filepath.Join(root, "openapi.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy, []byte("version: 1\nrules:\n  - {id: bad, type: timeout, operations: [missing], times: 1}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runCLI([]string{"run", "ambiguous-commit", "--agent", "scripted", "--manifest", manifest, "--db", db, "--chaos", policy}, &bytes.Buffer{}); err == nil {
		t.Fatal("invalid policy accepted")
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("database created: %v", err)
	}
}

func TestChaosCLISafeAndUnsafeRuns(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join("..", "..", "examples", "billing")
	manifest, db, policy := filepath.Join(dir, "manifest.json"), filepath.Join(dir, "world.db"), filepath.Join(dir, "chaos.yaml")
	if err := runCLI([]string{"build", filepath.Join(root, "openapi.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policy, []byte("version: 1\nrules:\n  - id: lost\n    type: timeout_after_commit\n    operations: [createRefund]\n    times: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, recovery := range []string{"safe", "unsafe"} {
		var out bytes.Buffer
		if err := runCLI([]string{"run", "ambiguous-commit", "--agent", "scripted", "--recovery", recovery, "--manifest", manifest, "--db", db, "--chaos", policy}, &out); err != nil {
			t.Fatal(err)
		}
		var result struct {
			Run struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"run"`
			Evaluation ReportForTest `json:"evaluation"`
			Analysis   struct {
				UnsafeRetry struct {
					Detected bool `json:"detected"`
				} `json:"unsafe_retry"`
			} `json:"analysis"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Run.Status != "completed" || result.Evaluation.Passed != (recovery == "safe") || result.Analysis.UnsafeRetry.Detected != (recovery == "unsafe") {
			t.Fatalf("%s: %s", recovery, out.String())
		}
		out.Reset()
		if err := runCLI([]string{"replay", result.Run.ID, "--manifest", manifest, "--db", db}, &out); err != nil {
			t.Fatalf("replay %s: %v", recovery, err)
		}
	}
}

func TestForkChaosPolicyReplacementStartsClean(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join("..", "..", "examples", "billing")
	manifest, db := filepath.Join(dir, "manifest.json"), filepath.Join(dir, "world.db")
	first, replacement := filepath.Join(dir, "first.yaml"), filepath.Join(dir, "replacement.yaml")
	if err := runCLI([]string{"build", filepath.Join(root, "openapi.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("version: 1\nrules:\n  - id: lost\n    type: timeout_after_commit\n    operations: [createRefund]\n    times: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, []byte("version: 1\nrules:\n  - id: fresh\n    type: latency\n    operations: [getCharge]\n    duration_ms: 250\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runCLI([]string{"run", "ambiguous-commit", "--agent", "scripted", "--manifest", manifest, "--db", db, "--chaos", first}, &out); err != nil {
		t.Fatal(err)
	}
	var rootResult struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(out.Bytes(), &rootResult); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runCLI([]string{"checkpoints", rootResult.Run.ID, "--manifest", manifest, "--db", db}, &out); err != nil {
		t.Fatal(err)
	}
	var points struct {
		Checkpoints []struct {
			EventSeq  int    `json:"event_seq"`
			EventType string `json:"event_type"`
		} `json:"checkpoints"`
	}
	if err := json.Unmarshal(out.Bytes(), &points); err != nil {
		t.Fatal(err)
	}
	seq := 0
	for _, point := range points.Checkpoints {
		if point.EventType == "tool.response" {
			seq = point.EventSeq
			break
		}
	}
	if seq == 0 {
		t.Fatal("missing tool response checkpoint")
	}
	out.Reset()
	if err := runCLI([]string{"fork", rootResult.Run.ID, "--at-event", strconv.Itoa(seq), "--manifest", manifest, "--db", db, "--chaos", replacement}, &out); err != nil {
		t.Fatal(err)
	}
	var forkResult struct {
		Fork struct {
			Run struct {
				ID string `json:"id"`
			} `json:"run"`
		} `json:"fork"`
	}
	if err := json.Unmarshal(out.Bytes(), &forkResult); err != nil {
		t.Fatal(err)
	}
	s, err := store.OpenReadOnly(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var policyJSON string
	if err := s.DB.QueryRow("SELECT policy_json FROM run_chaos WHERE run_id=?", forkResult.Fork.Run.ID).Scan(&policyJSON); err != nil || !strings.Contains(policyJSON, "fresh") {
		t.Fatalf("policy=%s err=%v", policyJSON, err)
	}
	var stateRows int
	if err := s.DB.QueryRow("SELECT count(*) FROM chaos_rule_state WHERE run_id=?", forkResult.Fork.Run.ID).Scan(&stateRows); err != nil || stateRows != 0 {
		t.Fatalf("state=%d err=%v", stateRows, err)
	}
	out.Reset()
	if err := runCLI([]string{"resume", forkResult.Fork.Run.ID, "--manifest", manifest, "--db", db}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runCLI([]string{"replay", forkResult.Fork.Run.ID, "--manifest", manifest, "--db", db}, &out); err != nil {
		t.Fatalf("replay replaced fork: %v", err)
	}
	out.Reset()
	if err := runCLI([]string{"inspect", forkResult.Fork.Run.ID, "--db", db}, &out); err != nil {
		t.Fatal(err)
	}
	var inspected struct {
		Analysis struct {
			SimulatedLatencyMS int `json:"simulated_latency_ms"`
		} `json:"analysis"`
	}
	if err := json.Unmarshal(out.Bytes(), &inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.Analysis.SimulatedLatencyMS != 250 {
		t.Fatalf("inspect=%s", out.String())
	}
	out.Reset()
	if err := runCLI([]string{"compare", rootResult.Run.ID, forkResult.Fork.Run.ID, "--db", db}, &out); err != nil {
		t.Fatal(err)
	}
	var compared struct {
		Child struct {
			Analysis struct {
				SimulatedLatencyMS int `json:"simulated_latency_ms"`
			} `json:"analysis"`
		} `json:"child"`
	}
	if err := json.Unmarshal(out.Bytes(), &compared); err != nil {
		t.Fatal(err)
	}
	if compared.Child.Analysis.SimulatedLatencyMS != 250 {
		t.Fatalf("compare=%s", out.String())
	}
	zeroGate := filepath.Join(dir, "zero-gate.yaml")
	if err := os.WriteFile(zeroGate, []byte("version: 1\nrules:\n  - id: gate\n    type: rate_limit\n    operations: [getCustomer]\n    after_calls: 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runCLI([]string{"fork", rootResult.Run.ID, "--at-event", strconv.Itoa(seq), "--manifest", manifest, "--db", db, "--chaos", zeroGate, "--steps", "5"}, &out); err != nil {
		t.Fatal(err)
	}
	var zeroResult struct {
		Fork struct {
			Run struct {
				ID string `json:"id"`
			} `json:"run"`
		} `json:"fork"`
	}
	if err := json.Unmarshal(out.Bytes(), &zeroResult); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runCLI([]string{"replay", zeroResult.Fork.Run.ID, "--manifest", manifest, "--db", db}, &out); err != nil {
		t.Fatalf("zero-gate replacement replay: %v", err)
	}
	out.Reset()
	if err := runCLI([]string{"fork", rootResult.Run.ID, "--at-event", strconv.Itoa(seq), "--manifest", manifest, "--db", db, "--model", "fixture-unsafe-v1", "--steps", "5"}, &out); err != nil {
		t.Fatal(err)
	}
	var unsafeFork struct {
		Fork struct {
			Run struct {
				ID string `json:"id"`
			} `json:"run"`
		} `json:"fork"`
	}
	if err := json.Unmarshal(out.Bytes(), &unsafeFork); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runCLI([]string{"inspect", unsafeFork.Fork.Run.ID, "--db", db}, &out); err != nil {
		t.Fatal(err)
	}
	var unsafeInspected struct {
		Analysis struct {
			UnsafeRetry struct {
				Detected bool `json:"detected"`
			} `json:"unsafe_retry"`
			InfrastructureFault struct {
				Detected bool `json:"detected"`
			} `json:"infrastructure_fault"`
		} `json:"analysis"`
	}
	if err := json.Unmarshal(out.Bytes(), &unsafeInspected); err != nil {
		t.Fatal(err)
	}
	if !unsafeInspected.Analysis.UnsafeRetry.Detected || !unsafeInspected.Analysis.InfrastructureFault.Detected {
		t.Fatalf("unsafe fork analysis=%+v", unsafeInspected.Analysis)
	}
}

type ReportForTest struct {
	Passed bool `json:"passed"`
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
		{name: "anthropic has no default model", agent: "anthropic", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveRunModel(tc.agent, tc.requested); got != tc.want {
				t.Fatalf("resolveRunModel(%q, %q) = %q, want %q", tc.agent, tc.requested, got, tc.want)
			}
		})
	}
}

func TestProviderPreflightDoesNotCreateDatabase(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("ANTHROPIC_MODEL", "")
	root := filepath.Join("..", "..", "examples", "billing")
	dir := t.TempDir()
	manifest, db := filepath.Join(dir, "manifest.json"), filepath.Join(dir, "world.db")
	if err := runCLI([]string{"build", filepath.Join(root, "openapi.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"anthropic":         {"run", "duplicate-charge", "--agent", "anthropic", "--model", "claude-test", "--manifest", manifest, "--db", db},
		"openai-compatible": {"run", "duplicate-charge", "--agent", "openai-compatible", "--model", "local", "--manifest", manifest, "--db", db},
	}
	for name, args := range cases {
		if err := runCLI(args, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := os.Stat(db); !os.IsNotExist(err) {
			t.Fatalf("%s created a database: %v", name, err)
		}
	}
}

func TestSecurityScenarioFromCLI(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	dir := t.TempDir()
	manifest, db := filepath.Join(dir, "manifest.json"), filepath.Join(dir, "world.db")
	support := filepath.Join(root, "security", "support-policy.yaml")
	overprivileged := filepath.Join(root, "security", "overprivileged-policy.yaml")
	invoke := func(args ...string) map[string]any {
		t.Helper()
		var out bytes.Buffer
		if err := runCLI(args, &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		var value map[string]any
		if err := json.Unmarshal(out.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	detected := func(result map[string]any, finding string) bool {
		return result["security"].(map[string]any)[finding].(map[string]any)["detected"] == true
	}
	invoke("build", filepath.Join(root, "company", "openapi.yaml"), "--bindings", filepath.Join(root, "company", "bindings.yaml"), "--out", manifest)

	invalid := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(invalid, []byte("version: 1\nprincipal: {id: ''}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runCLI([]string{"run", "prompt-injection-ticket", "--agent", "scripted", "--manifest", manifest, "--db", db, "--auth", invalid}, &bytes.Buffer{}); err == nil {
		t.Fatal("invalid authorization policy accepted")
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("database created before policy validation: %v", err)
	}

	paused := invoke("run", "prompt-injection-ticket", "--agent", "scripted", "--manifest", manifest, "--db", db, "--auth", support, "--steps", "2")
	run := paused["run"].(map[string]any)
	if run["status"] != "paused" || run["principal_id"] != "support-agent-1" {
		t.Fatalf("paused=%v", run)
	}
	id := run["id"].(string)
	completed := invoke("resume", id, "--agent", "scripted", "--manifest", manifest, "--db", db, "--steps", "10")
	if completed["run"].(map[string]any)["principal_id"] != "support-agent-1" || completed["evaluation"].(map[string]any)["passed"] != true {
		t.Fatalf("completed=%v", completed)
	}
	if !detected(completed, "blocked_violation") || detected(completed, "successful_violation") {
		t.Fatalf("security=%v", completed["security"])
	}
	inspected := invoke("inspect", id, "--db", db, "--manifest", manifest)
	if !detected(inspected, "blocked_violation") {
		t.Fatalf("inspect security=%v", inspected["security"])
	}
	invoke("replay", id, "--manifest", manifest, "--db", db)

	open := invoke("run", "prompt-injection-ticket", "--agent", "scripted", "--manifest", manifest, "--db", db, "--auth", overprivileged)
	if !detected(open, "successful_violation") || open["evaluation"].(map[string]any)["passed"] != false {
		t.Fatalf("overprivileged=%v", open)
	}

	points := invoke("checkpoints", id, "--manifest", manifest, "--db", db)["checkpoints"].([]any)
	seq := 0
	for _, point := range points {
		if p := point.(map[string]any); p["event_type"] == "tool.response" {
			seq = int(p["event_seq"].(float64))
			break
		}
	}
	forked := invoke("fork", id, "--at-event", strconv.Itoa(seq), "--manifest", manifest, "--db", db, "--auth", overprivileged, "--steps", "10")
	child := forked["fork"].(map[string]any)["run"].(map[string]any)
	if child["principal_id"] != "overprivileged-agent" {
		t.Fatalf("fork child=%v", child)
	}
	childID := child["id"].(string)
	if lineage := invoke("inspect", childID, "--db", db, "--manifest", manifest)["lineage"].(map[string]any); lineage["auth_replaced"] != true {
		t.Fatalf("lineage=%v", lineage)
	}
	invoke("replay", childID, "--manifest", manifest, "--db", db)

	plain := invoke("run", "company-routine", "--agent", "scripted", "--manifest", manifest, "--db", db)
	if plain["run"].(map[string]any)["principal_id"] != "local-unrestricted" {
		t.Fatalf("unrestricted run=%v", plain["run"])
	}
}

func TestEvaluateAssertionsFromCLI(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	dir := t.TempDir()
	manifest, billing, db := filepath.Join(dir, "company.json"), filepath.Join(dir, "billing.json"), filepath.Join(dir, "world.db")
	assertions := filepath.Join(root, "assertions", "prompt-injection-ticket.yaml")
	must := func(args ...string) *bytes.Buffer {
		t.Helper()
		var out bytes.Buffer
		if err := runCLI(args, &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return &out
	}
	runID := func(out *bytes.Buffer) string {
		t.Helper()
		var result struct {
			Run struct {
				ID string `json:"id"`
			} `json:"run"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Run.ID
	}
	must("build", filepath.Join(root, "company", "openapi.yaml"), "--bindings", filepath.Join(root, "company", "bindings.yaml"), "--out", manifest)
	must("build", filepath.Join(root, "billing", "openapi.yaml"), "--out", billing)

	missing := filepath.Join(dir, "missing.db")
	if err := runCLI([]string{"evaluate", "R-x", "--assertions", assertions, "--manifest", manifest, "--db", missing}, &bytes.Buffer{}); err == nil {
		t.Fatal("missing database accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("evaluate created a database: %v", err)
	}

	secure := runID(must("run", "prompt-injection-ticket", "--agent", "scripted", "--manifest", manifest, "--db", db, "--auth", filepath.Join(root, "security", "support-policy.yaml")))
	var report struct {
		RunID      string `json:"run_id"`
		Assertions struct {
			Passed  bool `json:"passed"`
			Results []struct {
				ID     string `json:"id"`
				Passed bool   `json:"passed"`
			} `json:"results"`
		} `json:"assertions"`
	}
	if err := json.Unmarshal(must("evaluate", secure, "--assertions", assertions, "--manifest", manifest, "--db", db).Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.RunID != secure || !report.Assertions.Passed || len(report.Assertions.Results) != 7 {
		t.Fatalf("report=%+v", report)
	}

	open := runID(must("run", "prompt-injection-ticket", "--agent", "scripted", "--manifest", manifest, "--db", db, "--auth", filepath.Join(root, "security", "overprivileged-policy.yaml")))
	var out bytes.Buffer
	if err := runCLI([]string{"evaluate", open, "--assertions", assertions, "--manifest", manifest, "--db", db}, &out); err == nil || !strings.Contains(err.Error(), "assertions failed") {
		t.Fatalf("failed assertions exited cleanly: %v", err)
	}
	if !strings.Contains(out.String(), `"passed":false`) {
		t.Fatalf("failure report not emitted: %s", out.String())
	}

	invalid := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(invalid, []byte("version: 1\nassertions: [{id: a, type: row_count, table: payroll, equals: 0}]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runCLI([]string{"evaluate", secure, "--assertions", invalid, "--manifest", manifest, "--db", db}, &bytes.Buffer{}); err == nil {
		t.Fatal("invalid assertions accepted")
	}
	if err := runCLI([]string{"evaluate", secure, "--assertions", filepath.Join(root, "assertions", "duplicate-charge.yaml"), "--manifest", billing, "--db", db}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "manifest mismatch") {
		t.Fatalf("manifest mismatch accepted: %v", err)
	}
	if err := runCLI([]string{"evaluate", secure, "--manifest", manifest, "--db", db}, &bytes.Buffer{}); err == nil {
		t.Fatal("missing --assertions accepted")
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
	definition, err := os.ReadFile(filepath.Join(root, "world.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(definition, []byte("to: billing.customers.id"), []byte("to: crm.accounts.id"), 1)
	loader, err := confinedWorldLoader(root)
	if err != nil {
		t.Fatal(err)
	}
	different, err := compiler.CompileWorld(changed, loader, behavior.Builtin())
	if err != nil {
		t.Fatal(err)
	}
	differentPath := filepath.Join(dir, "different.manifest.json")
	differentData, err := json.Marshal(different)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(differentPath, differentData, 0600); err != nil {
		t.Fatal(err)
	}
	if err := runCLI([]string{"resume", id, "--agent", "scripted", "--manifest", differentPath, "--db", db}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "manifest mismatch") {
		t.Fatalf("changed world resume error=%v", err)
	}
	completed := invoke("resume", id, "--agent", "scripted", "--manifest", manifest, "--db", db, "--steps", "30")
	if completed["run"].(map[string]any)["status"] != "completed" || completed["evaluation"].(map[string]any)["passed"] != true {
		t.Fatalf("completed=%v", completed)
	}
	verification := invoke("replay", id, "--manifest", manifest, "--db", db)
	if verification["verified"] != true {
		t.Fatalf("replay=%v", verification)
	}
	if err := runCLI([]string{"replay", id, "--manifest", differentPath, "--db", db}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "manifest mismatch") {
		t.Fatalf("changed world replay error=%v", err)
	}
}

func TestBuildWorldRunsWithRenamedOperation(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "company")
	dir := t.TempDir()
	definition, err := os.ReadFile(filepath.Join(root, "world.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	loader := func(path string) ([]byte, error) {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return nil, err
		}
		if path == "services/crm.openapi.yaml" || path == "services/crm.bindings.yaml" {
			data = bytes.ReplaceAll(data, []byte("crmGetAccount"), []byte("lookupAccount"))
		}
		return data, nil
	}
	manifest, err := compiler.CompileWorld(definition, loader, behavior.Builtin())
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "renamed.manifest.json")
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runCLI([]string{"run", "company-routine", "--agent", "scripted", "--manifest", manifestPath, "--db", filepath.Join(dir, "company.db"), "--steps", "30"}, &output); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Run struct {
			Status string `json:"status"`
		} `json:"run"`
		Evaluation struct {
			Passed bool `json:"passed"`
		} `json:"evaluation"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Run.Status != "completed" || !result.Evaluation.Passed {
		t.Fatalf("renamed operation run=%s", output.String())
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

func TestConfinedWorldLoaderRejectsNormalizedParentPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "services"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "services", "crm.yaml"), []byte("valid"), 0600); err != nil {
		t.Fatal(err)
	}
	load, err := confinedWorldLoader(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := load("services/../services/crm.yaml"); err == nil {
		t.Fatal("parent segment accepted after path normalization")
	}
	if _, err := load(filepath.Join(dir, "services", "crm.yaml")); err == nil {
		t.Fatal("absolute path accepted")
	}
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "services", "escape.yaml")); err == nil {
		if _, err := load("services/escape.yaml"); err == nil {
			t.Fatal("symlink outside definition directory accepted")
		}
	}
}

func TestCompanyScenarioAcceptsRenamedBoundOperation(t *testing.T) {
	manifest := compiler.Manifest{
		World:      &compiler.WorldMetadata{Definition: compiler.WorldDefinition{SeedProfile: "company-v1"}},
		Operations: []compiler.Operation{{ID: "lookupAccount", Behavior: "crm.getAccount"}},
	}
	if err := checkScenarioManifest(manifest, "company-routine"); err != nil {
		t.Fatalf("renamed bound operation rejected: %v", err)
	}
}

func TestCLICheckpointsListsCommittedBoundaries(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join("..", "..", "examples", "billing")
	manifest := filepath.Join(dir, "manifest.json")
	db := filepath.Join(dir, "world.db")
	if err := runCLI([]string{"build", filepath.Join(root, "openapi.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var started bytes.Buffer
	if err := runCLI([]string{"run", "duplicate-charge", "--agent", "scripted", "--manifest", manifest, "--db", db, "--steps", "1"}, &started); err != nil {
		t.Fatal(err)
	}
	var run struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(started.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runCLI([]string{"checkpoints", run.Run.ID, "--manifest", manifest, "--db", db}, &output); err != nil {
		t.Fatal(err)
	}
	var listed struct {
		RunID       string `json:"run_id"`
		Checkpoints []struct {
			ID       string `json:"id"`
			EventSeq int    `json:"event_seq"`
		} `json:"checkpoints"`
	}
	if err := json.Unmarshal(output.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.RunID != run.Run.ID || len(listed.Checkpoints) < 2 || listed.Checkpoints[0].ID == "" {
		t.Fatalf("checkpoints=%s", output.String())
	}
}

func TestCLIForksAndInspectsLineage(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join("..", "..", "examples", "billing")
	manifest := filepath.Join(dir, "manifest.json")
	db := filepath.Join(dir, "world.db")
	if err := runCLI([]string{"build", filepath.Join(root, "openapi.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var started bytes.Buffer
	if err := runCLI([]string{"run", "duplicate-charge", "--agent", "scripted", "--manifest", manifest, "--db", db, "--steps", "20"}, &started); err != nil {
		t.Fatal(err)
	}
	var parent struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(started.Bytes(), &parent); err != nil {
		t.Fatal(err)
	}
	var listed bytes.Buffer
	if err := runCLI([]string{"checkpoints", parent.Run.ID, "--manifest", manifest, "--db", db}, &listed); err != nil {
		t.Fatal(err)
	}
	var checkpoints struct {
		Checkpoints []struct {
			EventSeq  int    `json:"event_seq"`
			EventType string `json:"event_type"`
		} `json:"checkpoints"`
	}
	if err := json.Unmarshal(listed.Bytes(), &checkpoints); err != nil {
		t.Fatal(err)
	}
	var eventSeq int
	for _, point := range checkpoints.Checkpoints {
		if point.EventType == "model.response" {
			eventSeq = point.EventSeq
			break
		}
	}
	if eventSeq == 0 {
		t.Fatal("model checkpoint missing")
	}
	var output bytes.Buffer
	if err := runCLI([]string{"fork", parent.Run.ID, "--at-event", strconv.Itoa(eventSeq), "--manifest", manifest, "--db", db, "--steps", "20"}, &output); err != nil {
		t.Fatal(err)
	}
	var forkBody map[string]any
	if err := json.Unmarshal(output.Bytes(), &forkBody); err != nil {
		t.Fatal(err)
	}
	child := forkBody["fork"].(map[string]any)["run"].(map[string]any)
	childID := child["id"].(string)
	if childID == parent.Run.ID || child["status"] != "completed" {
		t.Fatalf("fork result=%s", output.String())
	}
	var inspected bytes.Buffer
	if err := runCLI([]string{"inspect", childID, "--db", db}, &inspected); err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(inspected.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	lineage := report["lineage"].(map[string]any)
	if lineage["parent_run_id"] != parent.Run.ID || int(lineage["fork_event_seq"].(float64)) != eventSeq {
		t.Fatalf("lineage=%v", lineage)
	}
	var compared bytes.Buffer
	if err := runCLI([]string{"compare", parent.Run.ID, childID, "--db", db}, &compared); err != nil {
		t.Fatal(err)
	}
	var comparison map[string]any
	if err := json.Unmarshal(compared.Bytes(), &comparison); err != nil {
		t.Fatal(err)
	}
	if comparison["parent_run_id"] != parent.Run.ID || comparison["child_run_id"] != childID {
		t.Fatalf("comparison=%s", compared.String())
	}
	var replayed bytes.Buffer
	if err := runCLI([]string{"replay", childID, "--manifest", manifest, "--db", db}, &replayed); err != nil {
		t.Fatal(err)
	}
	var replayReport map[string]any
	if err := json.Unmarshal(replayed.Bytes(), &replayReport); err != nil || replayReport["verified"] != true {
		t.Fatalf("fork replay=%s err=%v", replayed.String(), err)
	}
	if err := runCLI([]string{"fork", parent.Run.ID, "--at-event", "4", "--manifest", manifest, "--db", db}, &bytes.Buffer{}); err == nil {
		t.Fatal("mid-tool event accepted")
	}
	t.Setenv("OPENAI_API_KEY", "")
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	var before, after int
	if err := s.DB.QueryRow("SELECT count(*) FROM fork_lineage").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := runCLI([]string{"fork", parent.Run.ID, "--at-event", strconv.Itoa(eventSeq), "--manifest", manifest, "--db", db, "--agent", "openai", "--steps", "1"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("missing key error=%v", err)
	}
	if err := s.DB.QueryRow("SELECT count(*) FROM fork_lineage").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("invalid immediate continuation created a child: before=%d after=%d", before, after)
	}
}

func TestCLIReplayLegacyDatabaseWithoutLineageTable(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	db := filepath.Join(dir, "legacy.db")
	root := filepath.Join("..", "..", "examples", "billing")
	if err := runCLI([]string{"build", filepath.Join(root, "openapi.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var started bytes.Buffer
	if err := runCLI([]string{"run", "duplicate-charge", "--agent", "scripted", "--manifest", manifest, "--db", db, "--steps", "20"}, &started); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(started.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("DROP TABLE fork_lineage"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("DROP TABLE checkpoints"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var replayed bytes.Buffer
	if err := runCLI([]string{"replay", body.Run.ID, "--manifest", manifest, "--db", db}, &replayed); err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(replayed.Bytes(), &report); err != nil || report["verified"] != true {
		t.Fatalf("legacy replay=%s err=%v", replayed.String(), err)
	}
}

func TestCLIFailedForkDoesNotMigrateLegacyDatabase(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	db := filepath.Join(dir, "legacy.db")
	root := filepath.Join("..", "..", "examples", "billing")
	if err := runCLI([]string{"build", filepath.Join(root, "openapi.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var started bytes.Buffer
	if err := runCLI([]string{"run", "duplicate-charge", "--agent", "scripted", "--manifest", manifest, "--db", db, "--steps", "20"}, &started); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(started.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("DROP TABLE fork_lineage"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("DROP TABLE checkpoints"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE events SET payload=? WHERE run_id=? AND seq=2", `{"task":"tampered"}`, body.Run.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := runCLI([]string{"fork", body.Run.ID, "--at-event", "3", "--manifest", manifest, "--db", db}, &bytes.Buffer{}); err == nil {
		t.Fatal("tampered fork accepted")
	}
	readonly, err := store.OpenReadOnly(db)
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	var count int
	if err := readonly.DB.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('checkpoints','fork_lineage')").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed fork created %d lineage tables", count)
	}
}

type counterfactualReportForTest struct {
	RunID   string `json:"run_id"`
	Failure struct {
		Judge  string   `json:"judge"`
		Failed []string `json:"failed"`
	} `json:"failure"`
	Candidates []struct {
		EventSeq     int    `json:"event_seq"`
		Intervention string `json:"intervention"`
		CallID       string `json:"call_id"`
		Forks        int    `json:"forks"`
		Changed      int    `json:"changed"`
		Summary      string `json:"summary"`
		Evidence     []struct {
			RunID  string `json:"run_id"`
			Status string `json:"status"`
		} `json:"evidence"`
	} `json:"candidates"`
}

func TestCounterfactualFromCLI(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	dir := t.TempDir()
	billing, company, db := filepath.Join(dir, "billing.json"), filepath.Join(dir, "company.json"), filepath.Join(dir, "world.db")
	must := func(args ...string) []byte {
		t.Helper()
		var out bytes.Buffer
		if err := runCLI(args, &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.Bytes()
	}
	runID := func(out []byte) string {
		t.Helper()
		var result struct {
			Run struct {
				ID string `json:"id"`
			} `json:"run"`
		}
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatal(err)
		}
		return result.Run.ID
	}
	analyze := func(args ...string) counterfactualReportForTest {
		t.Helper()
		var report counterfactualReportForTest
		if err := json.Unmarshal(must(append([]string{"counterfactual"}, args...)...), &report); err != nil {
			t.Fatal(err)
		}
		return report
	}
	must("build", filepath.Join(root, "billing", "openapi.yaml"), "--out", billing)
	must("build", filepath.Join(root, "company", "openapi.yaml"), "--bindings", filepath.Join(root, "company", "bindings.yaml"), "--out", company)
	interventions := filepath.Join(root, "counterfactual", "ambiguous-commit.yaml")

	missing := filepath.Join(dir, "missing.db")
	if err := runCLI([]string{"counterfactual", "R-x", "--interventions", interventions, "--manifest", billing, "--db", missing}, &bytes.Buffer{}); err == nil {
		t.Fatal("missing database accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("counterfactual created a database: %v", err)
	}

	unsafe := runID(must("run", "ambiguous-commit", "--agent", "scripted", "--recovery", "unsafe", "--manifest", billing, "--db", db, "--chaos", filepath.Join(root, "chaos", "ambiguous-commit.yaml")))
	scenario := analyze(unsafe, "--interventions", interventions, "--manifest", billing, "--db", db)
	if scenario.RunID != unsafe || scenario.Failure.Judge != "scenario_evaluation" || len(scenario.Candidates) != 6 {
		t.Fatalf("report=%+v", scenario)
	}
	top := scenario.Candidates[0]
	if top.Intervention != "latency-instead-of-lost-response" || top.EventSeq != 4 || top.Changed != 1 || !strings.Contains(top.Summary, "corrected the final outcome in 1/1 forks") {
		t.Fatalf("top candidate=%+v", top)
	}
	must("replay", top.Evidence[0].RunID, "--manifest", billing, "--db", db)

	withAssertions := analyze(unsafe, "--interventions", interventions, "--assertions", filepath.Join(root, "assertions", "ambiguous-commit.yaml"), "--trials", "2", "--manifest", billing, "--db", db)
	changed := map[string]int{}
	for _, c := range withAssertions.Candidates {
		if c.Forks != 2 {
			t.Fatalf("candidate=%+v", c)
		}
		changed[strconv.Itoa(c.EventSeq)+" "+c.Intervention] = c.Changed
	}
	if withAssertions.Failure.Judge != "assertions" || changed["7 refund-delivered"] != 2 || changed["4 latency-instead-of-lost-response"] != 0 {
		t.Fatalf("assertion-judged changes=%v", changed)
	}

	for name, args := range map[string][]string{
		"missing interventions": {"counterfactual", unsafe, "--manifest", billing, "--db", db},
		"zero trials":           {"counterfactual", unsafe, "--interventions", interventions, "--trials", "0", "--manifest", billing, "--db", db},
		"zero steps":            {"counterfactual", unsafe, "--interventions", interventions, "--steps", "0", "--manifest", billing, "--db", db},
		"wrong manifest":        {"counterfactual", unsafe, "--interventions", filepath.Join(root, "counterfactual", "prompt-injection-ticket.yaml"), "--manifest", company, "--db", db},
		"passing run":           {"counterfactual", runID(must("run", "ambiguous-commit", "--agent", "scripted", "--manifest", billing, "--db", db, "--chaos", filepath.Join(root, "chaos", "ambiguous-commit.yaml"))), "--interventions", interventions, "--manifest", billing, "--db", db},
	} {
		if err := runCLI(args, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	open := runID(must("run", "prompt-injection-ticket", "--agent", "scripted", "--manifest", company, "--db", db, "--auth", filepath.Join(root, "security", "overprivileged-policy.yaml")))
	security := analyze(open, "--interventions", filepath.Join(root, "counterfactual", "prompt-injection-ticket.yaml"), "--assertions", filepath.Join(root, "assertions", "prompt-injection-ticket.yaml"), "--manifest", company, "--db", db)
	if len(security.Candidates) != 4 || security.Candidates[0].CallID != "security-1" || security.Candidates[0].Changed != 1 || security.Candidates[3].Changed != 0 {
		t.Fatalf("security report=%+v", security)
	}
	must("replay", security.Candidates[0].Evidence[0].RunID, "--manifest", company, "--db", db)
}

func TestTraceAndInspectSummaryFromCLI(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "billing")
	dir := t.TempDir()
	manifest, db := filepath.Join(dir, "manifest.json"), filepath.Join(dir, "world.db")
	var out bytes.Buffer
	if err := runCLI([]string{"build", filepath.Join(root, "openapi.yaml"), "--out", manifest}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runCLI([]string{"run", "duplicate-charge", "--agent", "scripted", "--fault", "listCharges", "--manifest", manifest, "--db", db}, &out); err != nil {
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
	missing := filepath.Join(dir, "missing.db")
	if err := runCLI([]string{"trace", result.Run.ID, "--db", missing}, &bytes.Buffer{}); err == nil {
		t.Fatal("missing database accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("trace created a database: %v", err)
	}
	if err := runCLI([]string{"trace", result.Run.ID, "--format", "yaml", "--db", db}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "unknown trace format") {
		t.Fatalf("bad format error=%v", err)
	}
	out.Reset()
	if err := runCLI([]string{"trace", result.Run.ID, "--db", db}, &out); err != nil {
		t.Fatal(err)
	}
	var traced struct {
		TraceID string `json:"trace_id"`
		Summary struct {
			ToolCalls       int `json:"tool_calls"`
			FailedToolCalls int `json:"failed_tool_calls"`
			Faults          int `json:"faults"`
			Retries         int `json:"retries"`
			TokenUsage      struct {
				Recorded bool `json:"recorded"`
			} `json:"token_usage"`
		} `json:"summary"`
		Root struct {
			Name string `json:"name"`
		} `json:"root"`
	}
	if err := json.Unmarshal(out.Bytes(), &traced); err != nil {
		t.Fatal(err)
	}
	if len(traced.TraceID) != 32 || traced.Root.Name != "run" || traced.Summary.ToolCalls != 5 || traced.Summary.FailedToolCalls != 1 || traced.Summary.Faults != 1 || traced.Summary.Retries != 1 || traced.Summary.TokenUsage.Recorded {
		t.Fatalf("trace=%s", out.String())
	}
	out.Reset()
	if err := runCLI([]string{"trace", result.Run.ID, "--format", "text", "--db", db}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "tool.call listCharges status=503 server_error") {
		t.Fatalf("text=%s", out.String())
	}
	out.Reset()
	if err := runCLI([]string{"trace", result.Run.ID, "--format", "otlp", "--db", db}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"traceId"`) || !json.Valid(out.Bytes()) {
		t.Fatalf("otlp=%s", out.String())
	}
	out.Reset()
	if err := runCLI([]string{"inspect", result.Run.ID, "--db", db}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), `{"summary":`) {
		t.Fatalf("inspect does not lead with the summary: %.120s", out.String())
	}
	if !strings.Contains(out.String(), `"events":`) || !strings.Contains(out.String(), `"evaluation":`) {
		t.Fatalf("inspect lost existing fields: %s", out.String())
	}
}
