package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"twinwright/internal/agent"
	"twinwright/internal/behavior"
	"twinwright/internal/bench"
	"twinwright/internal/compiler"
	"twinwright/internal/store"
	"twinwright/internal/worker"
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

func TestBenchCLIAndReportCompare(t *testing.T) {
	examples := filepath.Join("..", "..", "examples")
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "tiny.yaml")
	suiteYAML := []byte(`
version: 1
suite: tiny
cases:
  - id: billing-duplicate
    category: reasoning
    world: billing
    scenario: duplicate-charge
    assertions: assertions/duplicate-charge.yaml
    dimensions: [task]
  - id: ambiguous-unsafe
    category: safety
    world: billing
    scenario: ambiguous-commit
    chaos: chaos/ambiguous-commit.yaml
    assertions: assertions/ambiguous-commit.yaml
    recovery: unsafe
    dimensions: [task, safety, duplicate_effects]
`)
	if err := os.WriteFile(suitePath, suiteYAML, 0644); err != nil {
		t.Fatal(err)
	}
	reportA := filepath.Join(dir, "a.json")
	reportB := filepath.Join(dir, "b.json")
	var out bytes.Buffer
	if err := runCLI([]string{"bench", "--suite-file", suitePath, "--examples", examples, "--agent", "scripted", "--db-dir", filepath.Join(dir, "dbs"), "--out", reportA}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Task Success") || !strings.Contains(out.String(), `"suite":"tiny"`) {
		t.Fatalf("bench output=%s", out.String())
	}
	data, err := os.ReadFile(reportA)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportB, data, 0644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runCLI([]string{"compare", reportA, reportB}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"kind":"bench_report_compare"`) {
		t.Fatalf("compare=%s", out.String())
	}
}

func TestVersionCommandReportsBuildProvenance(t *testing.T) {
	var out bytes.Buffer
	if err := runCLI([]string{"version"}, &out); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("version output is not JSON: %v: %s", err, out.String())
	}
	// Existing fields stay; the new ones are additive.
	for _, key := range []string{"version", "go", "platform", "schema_version", "delivery", "git_modified"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("version output lost %q: %s", key, out.String())
		}
	}
	if _, ok := got["git_modified"].(bool); !ok {
		t.Fatalf("git_modified = %v, want a boolean", got["git_modified"])
	}
	if revision, present := got["git_revision"]; present {
		if text, _ := revision.(string); text == "" {
			t.Fatal("git_revision must be omitted rather than empty")
		}
	}
}

func TestBenchCLIReportIsSchemaVersionTwo(t *testing.T) {
	examples := filepath.Join("..", "..", "examples")
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "tiny.yaml")
	suiteYAML := []byte(`
version: 1
suite: tiny
cases:
  - id: billing-duplicate
    category: reasoning
    world: billing
    scenario: duplicate-charge
    assertions: assertions/duplicate-charge.yaml
    dimensions: [task]
`)
	if err := os.WriteFile(suitePath, suiteYAML, 0644); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(dir, "report.json")
	err := runCLI([]string{"bench", "--suite-file", suitePath, "--examples", examples, "--agent", "scripted", "--seed", "11", "--db-dir", filepath.Join(dir, "dbs"), "--out", reportPath}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	report, err := bench.DecodeReport(data)
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != 2 || report.ContentDigest == "" || report.Environment == nil {
		t.Fatalf("report is not self-describing: %s", data)
	}
	if report.Environment.Seed != 11 {
		t.Fatalf("Seed = %d, want the --seed value", report.Environment.Seed)
	}
	if report.Environment.Version == "" || report.Environment.GoVersion == "" {
		t.Fatalf("build metadata missing: %+v", report.Environment)
	}
}

func TestShadowCLIObserveOnly(t *testing.T) {
	examples := filepath.Join("..", "..", "examples")
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	if err := runCLI([]string{"build", filepath.Join(examples, "billing", "openapi.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runCLI([]string{
		"shadow", "--config", filepath.Join(examples, "shadow", "observe-only.yaml"),
		"--examples", examples, "--manifest", manifest, "--scenario", "duplicate-charge",
		"--agent", "scripted", "--work-dir", filepath.Join(dir, "work"),
	}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"experimental":true`) || !strings.Contains(out.String(), `"comparison"`) {
		t.Fatalf("%s", out.String())
	}
	if err := runCLI([]string{"shadow", "--config", filepath.Join(dir, "missing.yaml"), "--examples", examples, "--manifest", manifest}, &bytes.Buffer{}); err == nil {
		t.Fatal("missing config accepted")
	}
}

// Without a policy the shadow report has measured nothing about authorization,
// and has to say that rather than report an empty refusal list. With one, a
// proposed call the policy refuses appears even though the observed stream
// contains the same refund.
func TestShadowCLIScreensProposedActionsAgainstAPolicy(t *testing.T) {
	examples := filepath.Join("..", "..", "examples")
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	if err := runCLI([]string{"build", filepath.Join(examples, "billing", "openapi.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	base := []string{
		"shadow", "--config", filepath.Join(examples, "shadow", "observe-only.yaml"),
		"--examples", examples, "--manifest", manifest, "--scenario", "duplicate-charge",
		"--agent", "scripted",
	}
	var unscreened bytes.Buffer
	if err := runCLI(append(append([]string{}, base...), "--work-dir", filepath.Join(dir, "plain")), &unscreened); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unscreened.String(), `"evaluated":false`) || !strings.Contains(unscreened.String(), `"absent":"no principal policy`) {
		t.Fatalf("an unscreened report must name the absence: %s", unscreened.String())
	}

	// The policy the README documents for this world, so a broken fixture fails
	// here rather than for a reader.
	policy := filepath.Join(examples, "security", "billing-support-policy.yaml")
	var screened bytes.Buffer
	if err := runCLI(append(append([]string{}, base...), "--work-dir", filepath.Join(dir, "screened"), "--policy", policy), &screened); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"evaluated":true`, `"principal":"support-agent-1"`, `"operation_id":"createRefund"`, `"reason":"permission_not_granted"`, `"also_observed":true`} {
		if !strings.Contains(screened.String(), want) {
			t.Fatalf("screened report is missing %s: %s", want, screened.String())
		}
	}
	if err := runCLI(append(append([]string{}, base...), "--work-dir", filepath.Join(dir, "rejected"), "--policy", filepath.Join(dir, "absent.yaml")), &bytes.Buffer{}); err == nil {
		t.Fatal("missing policy file accepted")
	}
}

func TestContainerCLILocal(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "local.yaml")
	if err := os.WriteFile(cfgPath, []byte("version: 1\nname: local-sidecar\nruntime: local\nlabel: experimental\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runCLI([]string{"container", "start", "--config", cfgPath}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"experimental":true`) || !strings.Contains(out.String(), `"runtime":"local"`) {
		t.Fatalf("%s", out.String())
	}
	out.Reset()
	if err := runCLI([]string{"container", "status", "--config", cfgPath}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runCLI([]string{"container", "stop", "--config", cfgPath}, &out); err != nil {
		t.Fatal(err)
	}
}

// TestResumeRefusesARunLeasedByAWorker proves the local path takes the same
// fence as the fleet: a worker holding a live lease keeps the run, and resume
// refuses instead of executing alongside it.
func TestResumeRefusesARunLeasedByAWorker(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join("..", "..", "examples", "billing")
	manifest := filepath.Join(dir, "manifest.json")
	db := filepath.Join(dir, "world.db")
	if err := runCLI([]string{"build", filepath.Join(root, "openapi.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runCLI([]string{"run", "duplicate-charge", "--agent", "scripted", "--manifest", manifest, "--db", db, "--enqueue"}, &out); err != nil {
		t.Fatal(err)
	}
	var queued struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(out.Bytes(), &queued); err != nil || queued.RunID == "" {
		t.Fatalf("enqueue output: %v %s", err, out.String())
	}

	ctx := context.Background()
	s, err := store.OpenDSN(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ClaimRun(ctx, "worker-a", time.Minute, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	s.Close()

	err = runCLI([]string{"resume", queued.RunID, "--agent", "scripted", "--manifest", manifest, "--db", db}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "leased by") || !strings.Contains(err.Error(), "worker-a") {
		t.Fatalf("resume error=%v want a refusal naming worker-a", err)
	}
}

// TestLocalRunErrorDoesNotCallACompletedRunFailed pins the wording when the run
// finished but its lease could not be released: the work is durable, so the
// message must not say the run failed.
func TestLocalRunErrorDoesNotCallACompletedRunFailed(t *testing.T) {
	cause := errors.New("connection reset")
	left := localRunError("R-1", errors.New("boom"))
	if !strings.Contains(left.Error(), "run R-1 failed: ") {
		t.Fatalf("a failed run should read as failed: %v", left)
	}
	done := localRunError("R-1", errors.Join(worker.ErrLeaseNotReleased, cause))
	if strings.Contains(done.Error(), "failed") || !errors.Is(done, worker.ErrLeaseNotReleased) || !errors.Is(done, cause) {
		t.Fatalf("completed run must not read as failed and must keep its cause: %v", done)
	}
}

// TestLocalRunErrorSaysWhatActuallyHappened covers the two outcomes that are
// not failures. Ctrl-C leaves a resumable run, and a refusal because another
// owner holds the lease never started the run, so "failed" is wrong for both.
// A lost lease also wraps context.Canceled, because losing it cancels the run's
// context, and that one is a failure, so it must keep the failed wording.
func TestLocalRunErrorSaysWhatActuallyHappened(t *testing.T) {
	held := fmt.Errorf("%w: run R-1 is leased by %q until %s", store.ErrLeaseHeld, "worker-a", "2026-01-01T00:00:00Z")
	lost := fmt.Errorf("%w: lease on run R-1 was lost during local execution: %w", store.ErrFenced, context.Canceled)
	for _, tc := range []struct {
		name     string
		err      error
		contains []string
		excludes []string
		is       []error
	}{
		{
			name:     "interrupted",
			err:      context.Canceled,
			contains: []string{"run R-1 interrupted; resume it with `twinwright resume R-1`"},
			excludes: []string{"failed"},
			is:       []error{context.Canceled},
		},
		{
			name:     "interrupted with a release that also failed",
			err:      errors.Join(context.Canceled, errors.New("release lease on run R-1: connection reset")),
			contains: []string{"run R-1 interrupted; resume it with `twinwright resume R-1`", "connection reset"},
			excludes: []string{"failed"},
			is:       []error{context.Canceled},
		},
		{
			name:     "lease held by another owner",
			err:      held,
			contains: []string{"run R-1 not started: ", "leased by", "worker-a"},
			excludes: []string{"failed"},
			is:       []error{store.ErrLeaseHeld},
		},
		{
			name:     "lease lost during execution",
			err:      lost,
			contains: []string{"run R-1 failed: "},
			excludes: []string{"interrupted"},
			is:       []error{store.ErrFenced, context.Canceled},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := localRunError("R-1", tc.err)
			for _, want := range tc.contains {
				if !strings.Contains(got.Error(), want) {
					t.Errorf("error %q does not contain %q", got, want)
				}
			}
			for _, banned := range tc.excludes {
				if strings.Contains(got.Error(), banned) {
					t.Errorf("error %q contains %q", got, banned)
				}
			}
			for _, target := range tc.is {
				if !errors.Is(got, target) {
					t.Errorf("error %q no longer wraps %v", got, target)
				}
			}
		})
	}
}

func TestBenchCLITrialsFlagRepeatsLocalCases(t *testing.T) {
	examples := filepath.Join("..", "..", "examples")
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "tiny.yaml")
	suiteYAML := []byte(`
version: 1
suite: tiny
cases:
  - id: billing-duplicate
    category: reasoning
    world: billing
    scenario: duplicate-charge
    assertions: assertions/duplicate-charge.yaml
    dimensions: [task]
`)
	if err := os.WriteFile(suitePath, suiteYAML, 0644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args   []string
		trials int
	}{
		{nil, 1},
		{[]string{"--trials", "3"}, 3},
	} {
		reportPath := filepath.Join(dir, "report.json")
		args := append([]string{"bench", "--suite-file", suitePath, "--examples", examples, "--agent", "scripted", "--db-dir", filepath.Join(dir, "dbs"), "--out", reportPath}, tc.args...)
		if err := runCLI(args, &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(reportPath)
		if err != nil {
			t.Fatal(err)
		}
		report, err := bench.DecodeReport(data)
		if err != nil {
			t.Fatal(err)
		}
		got := report.Cases[0]
		if got.Trials != tc.trials || got.Passes != tc.trials || got.Status != "passed" {
			t.Fatalf("args %v: trials=%d passes=%d status=%s want %d passing trials", tc.args, got.Trials, got.Passes, got.Status, tc.trials)
		}
	}
}

func TestBenchCLIRefusesNonPositiveTrials(t *testing.T) {
	examples := filepath.Join("..", "..", "examples")
	for _, value := range []string{"0", "-1"} {
		err := runCLI([]string{"bench", "--examples", examples, "--agent", "scripted", "--db-dir", t.TempDir(), "--trials", value}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "--trials must be positive") {
			t.Errorf("--trials %s: err=%v want an error naming --trials", value, err)
		}
	}
}

// writeGateReport saves a minimal v2 bench report whose only case has the given
// status, so the gate's exit codes can be driven without running a suite.
func writeGateReport(t *testing.T, dir, name, suite, status string) string {
	t.Helper()
	passed := status == "passed"
	report := bench.Report{
		Suite: suite, SchemaVersion: 2, ContentDigest: "content-1",
		Cases: []bench.CaseResult{{
			ID: "refund", Status: status, Passed: passed, AsExpected: passed,
			Trials: 1, Mutations: 1, FailedChecks: []string{"refund_once"},
		}},
	}
	if passed {
		report.Cases[0].Passes = 1
		report.Cases[0].FailedChecks = nil
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGateExitCodes(t *testing.T) {
	dir := t.TempDir()
	good := writeGateReport(t, dir, "good.json", "standard", "passed")
	bad := writeGateReport(t, dir, "bad.json", "standard", "failed")
	other := writeGateReport(t, dir, "other.json", "other", "passed")
	notReport := filepath.Join(dir, "not-a-report.json")
	if err := os.WriteFile(notReport, []byte(`{"hello":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{"suite":"standard","schema_version":2,"cases":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"identical reports", []string{"--baseline", good, "--candidate", good}, 0},
		{"empty baseline", []string{"--baseline", empty, "--candidate", good}, 2},
		{"regression", []string{"--baseline", good, "--candidate", bad}, 1},
		{"missing candidate file", []string{"--baseline", good, "--candidate", filepath.Join(dir, "absent.json")}, 2},
		{"missing baseline file", []string{"--baseline", filepath.Join(dir, "absent.json"), "--candidate", good}, 2},
		{"not a bench report", []string{"--baseline", notReport, "--candidate", good}, 2},
		{"suite name mismatch", []string{"--baseline", good, "--candidate", other}, 2},
		{"baseline flag omitted", []string{"--candidate", good}, 2},
		{"unknown flag", []string{"--baseline", good, "--candidate", good, "--bogus"}, 2},
		{"invalid trajectory mode", []string{"--baseline", good, "--candidate", good, "--trajectory", "loud"}, 2},
		{"tolerance out of range", []string{"--baseline", good, "--candidate", good, "--tolerance", "2"}, 2},
		{"stray positional argument", []string{"--baseline", good, "--candidate", good, "extra"}, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runCLI(append([]string{"gate"}, tc.args...), &out)
			if got := exitCode(err); got != tc.want {
				t.Fatalf("exit code = %d (err %v), want %d", got, err, tc.want)
			}
			if tc.want == 0 && err != nil {
				t.Fatalf("exit 0 must carry no error, got %v", err)
			}
			if tc.want == 1 {
				var result struct {
					Passed      bool `json:"passed"`
					Regressions []struct {
						Kind string `json:"kind"`
					} `json:"regressions"`
				}
				if jsonErr := json.Unmarshal(out.Bytes(), &result); jsonErr != nil {
					t.Fatalf("stdout must be the Result JSON even on a regression: %v\n%s", jsonErr, out.String())
				}
				if result.Passed || len(result.Regressions) != 1 || result.Regressions[0].Kind != "newly_failing" {
					t.Fatalf("unexpected result: %+v", result)
				}
			}
		})
	}
}

func TestGateWritesMarkdownAndJUnit(t *testing.T) {
	dir := t.TempDir()
	good := writeGateReport(t, dir, "good.json", "standard", "passed")
	bad := writeGateReport(t, dir, "bad.json", "standard", "failed")
	markdown := filepath.Join(dir, "gate.md")
	junit := filepath.Join(dir, "gate.xml")
	var out bytes.Buffer
	err := runCLI([]string{"gate", "--baseline", good, "--candidate", bad, "--markdown", markdown, "--junit", junit}, &out)
	if exitCode(err) != 1 {
		t.Fatalf("want exit 1, got %d (%v)", exitCode(err), err)
	}
	if data, readErr := os.ReadFile(markdown); readErr != nil || !strings.Contains(string(data), "newly_failing") {
		t.Fatalf("markdown file: %v\n%s", readErr, data)
	}
	if data, readErr := os.ReadFile(junit); readErr != nil || !strings.Contains(string(data), "<testsuites") {
		t.Fatalf("junit file: %v\n%s", readErr, data)
	}

	// PATH "-" writes to the command's output after the Result JSON.
	out.Reset()
	err = runCLI([]string{"gate", "--baseline", good, "--candidate", bad, "--markdown", "-", "--junit", "-"}, &out)
	if exitCode(err) != 1 {
		t.Fatalf("want exit 1, got %d (%v)", exitCode(err), err)
	}
	for _, want := range []string{`"passed": false`, "FAILED", "<testsuites"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, out.String())
		}
	}
}

func TestExitCodeDefaultsToOne(t *testing.T) {
	if got := exitCode(errors.New("plain failure")); got != 1 {
		t.Fatalf("an ordinary error must keep exit code 1, got %d", got)
	}
	if got := exitCode(nil); got != 0 {
		t.Fatalf("no error is exit code 0, got %d", got)
	}
}

// Every command that can reach a provider registers its flags through
// providerFlags, so a rename or a changed default here would silently break
// scripts that already pass --base-url or --provider-timeout.
func TestProviderFlagsKeepTheirNamesAndDefaults(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "http://env.example/v1")
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cfg := providerFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if cfg.Timeout != 90*time.Second {
		t.Fatalf("--provider-timeout defaults to %v, want 90s", cfg.Timeout)
	}
	if cfg.Timeout != agent.DefaultCallTimeout {
		t.Fatalf("--provider-timeout defaults to %v, want agent.DefaultCallTimeout", cfg.Timeout)
	}
	if cfg.BaseURL != "http://env.example/v1" {
		t.Fatalf("--base-url defaults to %q, want $OPENAI_BASE_URL", cfg.BaseURL)
	}
	if cfg.MaxOutputTokens != 0 {
		t.Fatalf("--max-output-tokens defaults to %d, want 0 (the adapter default)", cfg.MaxOutputTokens)
	}

	fs = flag.NewFlagSet("t", flag.ContinueOnError)
	cfg = providerFlags(fs)
	if err := fs.Parse([]string{"--base-url", "http://flag.example/v1", "--provider-timeout", "5s", "--max-output-tokens", "321"}); err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "http://flag.example/v1" || cfg.Timeout != 5*time.Second || cfg.MaxOutputTokens != 321 {
		t.Fatalf("parsed config = %+v", *cfg)
	}
}

func TestSelectProviderCarriesTheOutputCeiling(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	base := providerConfig{Scenario: "duplicate-charge", Timeout: time.Second, MaxOutputTokens: 321}

	cfg := base
	cfg.Name, cfg.Model = "openai", "gpt-test"
	if p, err := selectProvider(cfg); err != nil {
		t.Fatal(err)
	} else if got, ok := p.(agent.OpenAIProvider); !ok || got.MaxOutputTokens != 321 {
		t.Fatalf("openai = %+v", p)
	}
	cfg = base
	cfg.Name, cfg.Model, cfg.BaseURL = "openai-compatible", "local", "http://127.0.0.1:1/v1"
	if p, err := selectProvider(cfg); err != nil {
		t.Fatal(err)
	} else if got, ok := p.(agent.ChatCompletionsProvider); !ok || got.MaxOutputTokens != 321 {
		t.Fatalf("openai-compatible = %+v", p)
	}
	cfg = base
	cfg.Name, cfg.Model = "anthropic", "claude-test"
	if p, err := selectProvider(cfg); err != nil {
		t.Fatal(err)
	} else if got, ok := p.(agent.AnthropicProvider); !ok || got.MaxOutputTokens != 321 {
		t.Fatalf("anthropic = %+v", p)
	}
	cfg.MaxOutputTokens = -1
	if _, err := selectProvider(cfg); err == nil {
		t.Fatal("a negative --max-output-tokens was accepted")
	}
}

// cliCassetteKey is long enough to be masked. Recording needs it; replay must not.
const cliCassetteKey = "cli-cassette-key-not-a-secret"

// cassetteRun is a duplicate-charge run recorded through the CLI against a
// scripted OpenAI-compatible server that is closed before the helper returns.
type cassetteRun struct {
	dir, manifest, db, cassette, runID string
}

// recordCLIRun runs `twinwright run --record-cassette` against a server that
// answers a tool call and then a final answer, and holds the real key in the
// environment while it does.
func recordCLIRun(t *testing.T) cassetteRun {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", cliCassetteKey)
	t.Setenv("OPENAI_BASE_URL", "")
	dir := t.TempDir()
	r := cassetteRun{dir: dir, manifest: buildBillingManifest(t, dir), db: filepath.Join(dir, "world.db"), cassette: filepath.Join(dir, "run.cassette")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		if req.URL.Path != "/v1/chat/completions" || req.Header.Get("Authorization") != "Bearer "+cliCassetteKey || !strings.Contains(string(body), `"max_tokens":77`) {
			t.Errorf("recording changed the request: path %s auth %q body %s", req.URL.Path, req.Header.Get("Authorization"), body)
		}
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(string(body), `"role":"tool"`) {
			w.Write([]byte(`{"id":"chatcmpl-1","model":"local-server-1","choices":[{"message":{"content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"getCharge","arguments":"{\"id\":\"CH-1002\"}"}}]},"finish_reason":"tool_calls"}]}`))
			return
		}
		w.Write([]byte(`{"id":"chatcmpl-2","model":"local-server-1","choices":[{"message":{"content":"Done"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	var out bytes.Buffer
	err := runCLI([]string{
		"run", "duplicate-charge", "--agent", "openai-compatible", "--model", "local", "--base-url", server.URL + "/v1",
		"--max-output-tokens", "77", "--record-cassette", r.cassette, "--manifest", r.manifest, "--db", r.db,
	}, &out)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	var result struct {
		Run struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"run"`
	}
	if err = json.Unmarshal(out.Bytes(), &result); err != nil || result.Run.Status != "completed" {
		t.Fatalf("run output: %v\n%s", err, out.String())
	}
	r.runID = result.Run.ID
	return r
}

// The server is gone and the key is not in the environment, so the verified
// report can only have come from the cassette. The ceiling is not repeated on the
// replay command line either: the cassette header carries it, because the
// request bodies only hash the same if the adapter is rebuilt with it.
func TestReplayCassetteFromCLI(t *testing.T) {
	r := recordCLIRun(t)
	t.Setenv("OPENAI_API_KEY", "")

	var out bytes.Buffer
	if err := runCLI([]string{"replay", r.runID, "--cassette", r.cassette, "--manifest", r.manifest, "--db", r.db}, &out); err != nil {
		t.Fatalf("replay: %v\n%s", err, out.String())
	}
	var report struct {
		Verified   bool   `json:"verified"`
		ModelTurns int    `json:"model_turns"`
		ToolCalls  int    `json:"tool_calls"`
		Divergence string `json:"divergence"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !report.Verified || report.ModelTurns != 2 || report.ToolCalls != 1 {
		t.Fatalf("report = %+v", report)
	}

	data, err := os.ReadFile(r.cassette)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	if want := `{"twinwright_cassette":1,"provider":"openai-compatible","model":"local","max_output_tokens":77}`; lines[0] != want {
		t.Fatalf("header = %s, want %s", lines[0], want)
	}
	if strings.Contains(string(data), cliCassetteKey) || strings.Contains(string(data), "Authorization") {
		t.Fatalf("the cassette holds a credential:\n%s", data)
	}

	// The same run still verifies without a cassette, from its own ledger.
	out.Reset()
	if err := runCLI([]string{"replay", r.runID, "--manifest", r.manifest, "--db", r.db}, &out); err != nil {
		t.Fatalf("replay without a cassette: %v", err)
	}
}

// A cassette answers one provider and model. The refusal names both sides, and
// an unreadable or empty cassette is refused before any replay starts.
func TestReplayCassetteRefusesWhatDoesNotMatchTheRun(t *testing.T) {
	r := recordCLIRun(t)
	t.Setenv("OPENAI_API_KEY", "")
	data, err := os.ReadFile(r.cassette)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	header := lines[0]
	cases := []struct{ name, content, want string }{
		{"another model", strings.Replace(header, `"model":"local"`, `"model":"other"`, 1) + "\n" + strings.Join(lines[1:], "\n") + "\n", "openai-compatible/other"},
		{"another provider", strings.Replace(header, `"openai-compatible"`, `"anthropic"`, 1) + "\n" + strings.Join(lines[1:], "\n") + "\n", "anthropic/local"},
		{"another version", strings.Replace(header, `"twinwright_cassette":1`, `"twinwright_cassette":2`, 1) + "\n", "version 2"},
		{"no exchanges", header + "\n", "no exchanges"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "edited.cassette")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			err := runCLI([]string{"replay", r.runID, "--cassette", path, "--manifest", r.manifest, "--db", r.db}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one that says %q", err, tc.want)
			}
			if tc.name == "another model" && !strings.Contains(err.Error(), "openai-compatible/local") {
				t.Fatalf("the refusal does not name the run's own provider and model: %v", err)
			}
		})
	}
	missing := filepath.Join(t.TempDir(), "absent.cassette")
	if err := runCLI([]string{"replay", r.runID, "--cassette", missing, "--manifest", r.manifest, "--db", r.db}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "absent.cassette") {
		t.Fatalf("a missing cassette: %v", err)
	}
}

// Both flags are for a provider that makes HTTP calls, and each is on exactly one
// command: a cassette holds one run's exchanges, and bench, worker, fork and
// resume run many runs or continue one.
func TestCassetteFlagsAreRefusedWhereTheyMeanNothing(t *testing.T) {
	dir := t.TempDir()
	manifest := buildBillingManifest(t, dir)
	db := filepath.Join(dir, "world.db")
	cassette := filepath.Join(dir, "x.cassette")

	err := runCLI([]string{"run", "duplicate-charge", "--agent", "scripted", "--record-cassette", cassette, "--manifest", manifest, "--db", db}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--record-cassette") {
		t.Fatalf("a scripted run was allowed to record a cassette: %v", err)
	}
	if _, statErr := os.Stat(cassette); statErr == nil {
		t.Fatal("a refused --record-cassette still created the file")
	}

	err = runCLI([]string{"run", "duplicate-charge", "--agent", "openai-compatible", "--model", "local", "--base-url", "http://127.0.0.1:1/v1", "--enqueue", "--record-cassette", cassette, "--manifest", manifest, "--db", db}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--enqueue") || !strings.Contains(err.Error(), "--record-cassette") {
		t.Fatalf("an enqueued run was allowed to record a cassette: %v", err)
	}
	if _, statErr := os.Stat(cassette); statErr == nil {
		t.Fatal("a refused --record-cassette still created the file")
	}

	var out bytes.Buffer
	if err = runCLI([]string{"run", "duplicate-charge", "--agent", "scripted", "--manifest", manifest, "--db", db}, &out); err != nil {
		t.Fatal(err)
	}
	var started struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err = json.Unmarshal(out.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	id := started.Run.ID
	if err = runCLI([]string{"replay", id, "--cassette", cassette, "--manifest", manifest, "--db", db}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--cassette") {
		t.Fatalf("a scripted run was replayed from a cassette: %v", err)
	}

	for _, args := range [][]string{
		{"resume", id, "--record-cassette", cassette},
		{"replay", id, "--record-cassette", cassette},
		{"run", "duplicate-charge", "--cassette", cassette},
		{"fork", id, "--at-event", "1", "--record-cassette", cassette},
		{"worker", "--record-cassette", cassette},
	} {
		err := runCLI(append(args, "--manifest", manifest, "--db", db), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "not defined") {
			t.Errorf("%v: err = %v, want the flag to be unknown to that command", args[:2], err)
		}
	}
}

// A recording owns a file, so selectProvider, which has nowhere to hand it back,
// refuses to open one rather than leak it.
func TestSelectProviderDoesNotOpenARecording(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.cassette")
	_, err := selectProvider(providerConfig{Name: "openai-compatible", Model: "local", BaseURL: "http://127.0.0.1:1/v1", Timeout: time.Second, RecordCassette: path})
	if err == nil {
		t.Fatal("selectProvider accepted a recording it cannot close")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("the refused recording still created its file")
	}
}
