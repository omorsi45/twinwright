package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"twinwright/internal/gate"
	"twinwright/internal/store"
)

const approvedBaseline = "../../examples/bench/baselines/standard-scripted.json"

// The gate is only worth wiring into CI if it can be seen to fail. The healthy
// fixture must pass against the approved baseline and the regressed fixture,
// which claims success instead of issuing the first write, must be blocked with
// a regression on the case that needed that write.
func TestRegressedBenchFailsTheGate(t *testing.T) {
	examples := filepath.Join("..", "..", "examples")
	dir := t.TempDir()
	for _, tc := range []struct {
		model    string
		wantExit int
	}{
		{"fixture-v1", 0},
		{"fixture-regressed-v1", 1},
	} {
		t.Run(tc.model, func(t *testing.T) {
			candidate := filepath.Join(dir, tc.model+".json")
			args := []string{"bench", "--suite", "standard", "--agent", "scripted", "--model", tc.model,
				"--examples", examples, "--db-dir", filepath.Join(dir, tc.model+"-dbs"), "--out", candidate}
			if err := runCLI(args, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err := runCLI([]string{"gate", "--baseline", approvedBaseline, "--candidate", candidate}, &out)
			if got := exitCode(err); got != tc.wantExit {
				t.Fatalf("exit=%d want %d err=%v\n%s", got, tc.wantExit, err, out.String())
			}
			var result gate.Result
			if jsonErr := json.Unmarshal(out.Bytes(), &result); jsonErr != nil {
				t.Fatalf("gate output is not a Result: %v\n%s", jsonErr, out.String())
			}
			if tc.wantExit == 0 {
				// The approved baseline is the healthy fixture's own report, so
				// it must compare clean. The gate would exit 0 on a warning, so
				// this is the repository's own, stricter rule (ADR 0030, "Updating
				// the baseline"): a warning here is drift nobody approved, and the
				// fix is to regenerate the baseline with the change that caused it.
				if len(result.Warnings) != 0 {
					t.Fatalf("healthy fixture compared with warnings, which the gate tolerates and this repository does not; regenerate the baseline with the change that caused them: %+v", result.Warnings)
				}
				return
			}
			for _, r := range result.Regressions {
				if r.Case == "billing-duplicate-charge" {
					return
				}
			}
			t.Fatalf("no regression for billing-duplicate-charge: %+v", result.Regressions)
		})
	}
}

// The approved baseline is read by CI, so it has to stay a schema-2 report from
// the deterministic fixture.
func TestApprovedBaselineIsADeterministicSchemaTwoReport(t *testing.T) {
	report, err := readBenchReport(approvedBaseline)
	if err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != 2 || report.Agent != "scripted" || len(report.Cases) == 0 {
		t.Fatalf("schema=%d agent=%q model=%q cases=%d", report.SchemaVersion, report.Agent, report.Model, len(report.Cases))
	}
}

// --fail-on-eval turns a failed evaluation into a non-zero exit without
// changing the default, which still prints the failure and succeeds.
func TestRunFailOnEval(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join("..", "..", "examples")
	manifest := filepath.Join(dir, "manifest.json")
	if err := runCLI([]string{"build", filepath.Join(root, "billing", "openapi.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	// The unsafe recovery retries a refund whose response was lost after it
	// committed, so the chaos policy that loses that response is what makes the
	// evaluation fail.
	policy := filepath.Join(dir, "chaos.yaml")
	if err := os.WriteFile(policy, []byte("version: 1\nrules:\n  - id: lost\n    type: timeout_after_commit\n    operations: [createRefund]\n    times: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := func(db string, extra ...string) []string {
		return append([]string{"run", "ambiguous-commit", "--agent", "scripted", "--recovery", "unsafe",
			"--manifest", manifest, "--db", filepath.Join(dir, db), "--chaos", policy}, extra...)
	}
	var plain bytes.Buffer
	if err := runCLI(args("plain.db"), &plain); err != nil {
		t.Fatalf("without the flag a failed evaluation must not be an error: %v", err)
	}
	var result struct {
		Evaluation struct {
			Passed bool `json:"passed"`
		} `json:"evaluation"`
	}
	if err := json.Unmarshal(plain.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Evaluation.Passed {
		t.Fatalf("the unsafe recovery was expected to fail its evaluation:\n%s", plain.String())
	}
	// The flag only changes the exit code of a failed evaluation: a passing one
	// still returns nil, and the safe recovery under the same chaos passes.
	var passing bytes.Buffer
	if err := runCLI([]string{"run", "ambiguous-commit", "--agent", "scripted", "--recovery", "safe",
		"--manifest", manifest, "--db", filepath.Join(dir, "passing.db"), "--chaos", policy, "--fail-on-eval"}, &passing); err != nil {
		t.Fatalf("--fail-on-eval failed a passing evaluation: %v", err)
	}
	var verdict struct {
		Evaluation struct {
			Passed bool `json:"passed"`
		} `json:"evaluation"`
	}
	if err := json.Unmarshal(passing.Bytes(), &verdict); err != nil || !verdict.Evaluation.Passed {
		t.Fatalf("the safe recovery was expected to pass its evaluation (err=%v):\n%s", err, passing.String())
	}
	var flagged bytes.Buffer
	err := runCLI(args("flagged.db", "--fail-on-eval"), &flagged)
	if err == nil {
		t.Fatal("--fail-on-eval accepted a failed evaluation")
	}
	if flagged.Len() == 0 {
		t.Fatal("the result must still be printed before the command fails")
	}
}

// Enqueueing hands the run to a worker, so there is no evaluation to fail on. The
// combination has to be refused, naming both flags, rather than silently exit 0.
func TestRunRefusesFailOnEvalWithEnqueue(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	root := filepath.Join("..", "..", "examples")
	if err := runCLI([]string{"build", filepath.Join(root, "billing", "openapi.yaml"), "--out", manifest}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "world.db")
	err := runCLI([]string{"run", "duplicate-charge", "--agent", "scripted", "--manifest", manifest, "--db", db, "--enqueue", "--fail-on-eval"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("--enqueue with --fail-on-eval was accepted")
	}
	for _, flagName := range []string{"--enqueue", "--fail-on-eval"} {
		if !strings.Contains(err.Error(), flagName) {
			t.Fatalf("error %q does not name %s", err, flagName)
		}
	}
	if _, statErr := os.Stat(db); !os.IsNotExist(statErr) {
		t.Fatalf("the database was created before the flags were refused: %v", statErr)
	}
}

// The wrapper classifies by the manifest, so under an enforced policy that hides
// the message write it still stops before it, on the run path and on the resume
// path. The denied read of another customer's record that precedes it is a read
// and is left alone, so the evidence is the absence of the write, not of every
// security finding.
func TestRegressedAgentStopsBeforeHiddenWritesFromTheCLI(t *testing.T) {
	root := filepath.Join("..", "..", "examples")
	dir := t.TempDir()
	manifest, db := filepath.Join(dir, "manifest.json"), filepath.Join(dir, "world.db")
	support := filepath.Join(root, "security", "support-policy.yaml")
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
	invoke("build", filepath.Join(root, "company", "openapi.yaml"), "--bindings", filepath.Join(root, "company", "bindings.yaml"), "--out", manifest)
	s, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	writeAttempts := func(result map[string]any) int {
		t.Helper()
		run := result["run"].(map[string]any)
		if run["status"] != "completed" {
			t.Fatalf("run=%v", run)
		}
		var n int
		if err := s.DB.QueryRow("SELECT count(*) FROM tool_results WHERE run_id=? AND operation_id=?", run["id"], "messagePostMessage").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	args := []string{"run", "prompt-injection-ticket", "--agent", "scripted", "--model", "fixture-regressed-v1", "--manifest", manifest, "--db", db, "--auth", support}
	if n := writeAttempts(invoke(args...)); n != 0 {
		t.Fatalf("run attempted the hidden write %d time(s)", n)
	}
	paused := invoke(append(args, "--steps", "2")...)
	id := paused["run"].(map[string]any)["id"].(string)
	if n := writeAttempts(invoke("resume", id, "--manifest", manifest, "--db", db, "--steps", "10")); n != 0 {
		t.Fatalf("resume attempted the hidden write %d time(s)", n)
	}
}
