package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"twinwright/internal/perf"
)

func perfExamples() string { return filepath.Join("..", "..", "examples") }

func TestPerfCLIWritesTheReport(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "perf.json")
	var out bytes.Buffer
	err := runCLI([]string{
		"perf", "--examples", perfExamples(),
		"--workers", "1,2", "--runs", "2", "--trials", "1",
		"--lease-ttls", "1s", "--replay-runs", "1", "--seed", "7",
		"--out", outPath,
	}, &out)
	if err != nil {
		t.Fatalf("perf: %v\n%s", err, out.String())
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("the report was not written: %v", err)
	}
	if !bytes.HasSuffix(data, []byte("}\n")) {
		t.Errorf("the report must end with a newline after the closing brace, got %q", data[max(0, len(data)-8):])
	}
	if !bytes.Contains(data, []byte("\n  \"")) {
		t.Errorf("the report is not indented:\n%s", data)
	}
	if bytes.Contains(bytes.ToLower(data), []byte("dsn")) {
		t.Errorf("the report mentions a DSN:\n%s", data)
	}

	var report perf.Report
	if err = json.Unmarshal(data, &report); err != nil {
		t.Fatalf("the report is not JSON: %v", err)
	}
	if report.SchemaVersion != 1 || report.Backend != "sqlite" {
		t.Errorf("schema_version=%d backend=%q want 1 and sqlite", report.SchemaVersion, report.Backend)
	}
	if report.Environment.Seed != 7 || report.Config.Seed != 7 {
		t.Errorf("--seed 7 was not recorded: environment %d, config %d", report.Environment.Seed, report.Config.Seed)
	}
	if got := report.Config.Workers; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("config workers=%v want [1 2]", got)
	}
	if len(report.Throughput) != 2 || len(report.Recovery) != 1 || report.Replay == nil || report.CrashMatrix == nil {
		t.Fatalf("a section is missing: %+v", report)
	}
	if report.Recovery[0].LeaseTTLMS != 1000 || report.Recovery[0].DuplicateMutations != 0 {
		t.Errorf("recovery row is wrong: %+v", report.Recovery[0])
	}
	if report.Replay.Verified != 1 || report.CrashMatrix.Violations != 0 {
		t.Errorf("replay %+v, crash matrix %+v", *report.Replay, *report.CrashMatrix)
	}

	text := strings.ToLower(out.String())
	for _, want := range []string{"throughput", "recovery", "replay", "crash matrix"} {
		if !strings.Contains(text, want) {
			t.Errorf("the printed summary does not mention %q:\n%s", want, out.String())
		}
	}
}

func TestPerfCLIRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no --out", nil},
		{"empty worker list", []string{"--workers", ""}},
		{"empty worker item", []string{"--workers", "1,,2"}},
		{"trailing comma in workers", []string{"--workers", "1,2,"}},
		{"zero workers", []string{"--workers", "0"}},
		{"negative workers", []string{"--workers", "2,-1"}},
		{"non-numeric workers", []string{"--workers", "many"}},
		{"empty lease ttl item", []string{"--lease-ttls", "1s,,2s"}},
		{"zero lease ttl", []string{"--lease-ttls", "0s"}},
		{"negative lease ttl", []string{"--lease-ttls", "-1s"}},
		{"lease ttl under the store minimum", []string{"--lease-ttls", "500ms"}},
		{"unparsable lease ttl", []string{"--lease-ttls", "soon"}},
		{"unknown workload", []string{"--workloads", "throughput,latency"}},
		{"empty workload item", []string{"--workloads", "throughput,"}},
		{"repeated workload", []string{"--workloads", "replay,replay"}},
		{"zero runs", []string{"--runs", "0"}},
		{"zero trials", []string{"--trials", "0"}},
		{"zero replay runs", []string{"--replay-runs", "0"}},
		{"zero seed", []string{"--seed", "0"}},
		{"unknown flag", []string{"--bogus"}},
		{"positional argument", []string{"extra"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outPath := filepath.Join(t.TempDir(), "perf.json")
			args := append([]string{"perf", "--examples", perfExamples()}, tc.args...)
			if tc.name != "no --out" {
				args = append(args, "--out", outPath)
			}
			var out bytes.Buffer
			err := runCLI(args, &out)
			if err == nil {
				t.Fatalf("perf accepted %v", tc.args)
			}
			if got := exitCode(err); got != exitBadInput {
				t.Errorf("exit code=%d (%v) want %d", got, err, exitBadInput)
			}
			if _, statErr := os.Stat(outPath); statErr == nil {
				t.Error("bad input still wrote a report")
			}
			if out.Len() != 0 {
				t.Errorf("bad input printed a summary: %s", out.String())
			}
		})
	}
}

// A report that records a duplicated mutation or a recovery violation is the
// finding the command exists to produce, so the file must exist before the
// command fails, or the failure would leave nothing to look at.
func TestPerfFinishWritesTheReportBeforeFailingOnAProblem(t *testing.T) {
	tests := []struct {
		name     string
		report   perf.Report
		wantExit int
	}{
		{"clean", perf.Report{SchemaVersion: 1, Backend: "sqlite"}, 0},
		{"duplicated mutation", perf.Report{SchemaVersion: 1, Backend: "sqlite",
			Recovery: []perf.RecoveryResult{{LeaseTTLMS: 1000, DuplicateMutations: 1}}}, 1},
		{"crash matrix violation", perf.Report{SchemaVersion: 1, Backend: "sqlite",
			CrashMatrix: &perf.MatrixResult{Scenario: "duplicate-charge", Cases: 9, Violations: 2}}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outPath := filepath.Join(t.TempDir(), "perf.json")
			var out bytes.Buffer
			err := finishPerf(tc.report, outPath, &out)
			if got := exitCode(err); got != tc.wantExit {
				t.Fatalf("exit code=%d (%v) want %d", got, err, tc.wantExit)
			}
			if tc.wantExit == 0 && err != nil {
				t.Fatalf("exit 0 must carry no error, got %v", err)
			}
			data, readErr := os.ReadFile(outPath)
			if readErr != nil {
				t.Fatalf("the report was not written: %v", readErr)
			}
			var decoded perf.Report
			if jsonErr := json.Unmarshal(data, &decoded); jsonErr != nil {
				t.Fatalf("the report is not JSON: %v", jsonErr)
			}
			if out.Len() == 0 {
				t.Error("no summary was printed")
			}
			if tc.wantExit == 1 && !strings.Contains(err.Error(), "perf") {
				t.Errorf("the failure does not say what failed: %v", err)
			}
		})
	}
}

func TestPerfCLINeverPrintsTheDSN(t *testing.T) {
	// The first is well formed and refused by the network; the rest do not parse,
	// which is the only kind whose unscrubbed error carries the password, so the
	// test fails when the command stops scrubbing.
	for name, tc := range map[string]struct{ dsn, password string }{
		"refused connection": {"postgres://perf:hunter2-secret@127.0.0.1:1/perf?sslmode=disable", "hunter2-secret"},
		"slash":              {"postgres://perf:hunter2/secret@127.0.0.1:1/perf?sslmode=disable", "hunter2/secret"},
		"hash":               {"postgres://perf:hunter2#secret@127.0.0.1:1/perf?sslmode=disable", "hunter2#secret"},
		"question mark":      {"postgres://perf:hunter2?secret@127.0.0.1:1/perf?sslmode=disable", "hunter2?secret"},
		"bad escape":         {"postgres://perf:hun%zzer2-secret@127.0.0.1:1/perf?sslmode=disable", "hun%zzer2-secret"},
	} {
		t.Run(name, func(t *testing.T) {
			outPath := filepath.Join(t.TempDir(), "perf.json")
			var out bytes.Buffer
			err := runCLI([]string{
				"perf", "--examples", perfExamples(), "--db", tc.dsn,
				"--workloads", "replay", "--replay-runs", "1", "--out", outPath,
			}, &out)
			if err == nil {
				t.Fatal("perf succeeded against a DSN nothing can serve")
			}
			if got := exitCode(err); got != 1 {
				t.Errorf("exit code=%d want 1 for a run that could not start", got)
			}
			text := err.Error() + "\n" + out.String()
			if strings.Contains(text, tc.dsn) {
				t.Errorf("the DSN leaked into the output: %s", text)
			}
			for i := 0; i+4 <= len(tc.password); i++ {
				if piece := tc.password[i : i+4]; strings.Contains(text, piece) {
					t.Errorf("a piece of the password (%q) leaked into the output: %s", piece, text)
					break
				}
			}
			if _, statErr := os.Stat(outPath); statErr == nil {
				t.Error("a run that could not start still wrote a report")
			}
		})
	}
}

func TestUsageNamesPerf(t *testing.T) {
	err := runCLI(nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "|perf|") {
		t.Fatalf("the usage string must list perf: %v", err)
	}
}
