package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"twinwright/internal/gate"
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
	var flagged bytes.Buffer
	err := runCLI(args("flagged.db", "--fail-on-eval"), &flagged)
	if err == nil {
		t.Fatal("--fail-on-eval accepted a failed evaluation")
	}
	if flagged.Len() == 0 {
		t.Fatal("the result must still be printed before the command fails")
	}
}
