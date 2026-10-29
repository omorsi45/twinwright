package perf

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"twinwright/internal/buildinfo"
	"twinwright/internal/crashmatrix"
	"twinwright/internal/dispatch"
	"twinwright/internal/pgtest"
	"twinwright/internal/store"
	"twinwright/internal/worker"
)

const examplesRoot = "../../examples"

func duplicateCharge(t *testing.T) crashmatrix.Scenario {
	t.Helper()
	scenarios, err := crashmatrix.Scenarios(examplesRoot)
	if err != nil {
		t.Fatalf("scenarios: %v", err)
	}
	for _, sc := range scenarios {
		if sc.Name == "duplicate-charge" {
			return sc
		}
	}
	t.Fatal("no duplicate-charge scenario")
	return crashmatrix.Scenario{}
}

func openSQLite(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "perf.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// keysNamed collects every JSON object key anywhere in v whose lower-cased name
// contains needle, so a nested field cannot hide a connection string.
func keysNamed(v any, needle string) []string {
	var found []string
	switch typed := v.(type) {
	case map[string]any:
		for key, value := range typed {
			if strings.Contains(strings.ToLower(key), needle) {
				found = append(found, key)
			}
			found = append(found, keysNamed(value, needle)...)
		}
	case []any:
		for _, value := range typed {
			found = append(found, keysNamed(value, needle)...)
		}
	}
	return found
}

func TestPerfReportShape(t *testing.T) {
	report, err := Run(context.Background(), Config{
		Examples:   examplesRoot,
		Workers:    []int{1, 2},
		Runs:       4,
		Trials:     1,
		LeaseTTLs:  []time.Duration{time.Second},
		ReplayRuns: 2,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.SchemaVersion != 1 {
		t.Errorf("schema_version=%d want 1", report.SchemaVersion)
	}
	if report.Backend != "sqlite" {
		t.Errorf("backend=%q want sqlite", report.Backend)
	}
	env := report.Environment
	if env.GOOS != runtime.GOOS || env.GOARCH != runtime.GOARCH || env.NumCPU < 1 {
		t.Errorf("environment does not describe this machine: %+v", env)
	}
	if env.GoVersion == "" || env.Version == "" {
		t.Errorf("environment has no build info: %+v", env)
	}
	if env.Seed != 42 {
		t.Errorf("environment seed=%d want the default 42", env.Seed)
	}
	if _, err := time.Parse(time.RFC3339, env.StartedAt); err != nil {
		t.Errorf("environment started_at=%q is not RFC 3339: %v", env.StartedAt, err)
	}

	if len(report.Throughput) != 2 {
		t.Fatalf("throughput has %d rows, want one per worker count", len(report.Throughput))
	}
	for i, want := range []int{1, 2} {
		row := report.Throughput[i]
		if row.Workers != want || row.Runs != 4 {
			t.Errorf("throughput[%d]=%d workers %d runs, want %d workers 4 runs", i, row.Workers, row.Runs, want)
		}
		if row.RunsPerSecond.Min <= 0 {
			t.Errorf("throughput[%d] runs_per_second.min=%v want > 0", i, row.RunsPerSecond.Min)
		}
		if s := row.RunsPerSecond; s.Min > s.Median || s.Median > s.Max {
			t.Errorf("throughput[%d] runs_per_second is not ordered: %+v", i, s)
		}
		if row.WallMS.Min <= 0 {
			t.Errorf("throughput[%d] wall_ms.min=%v want > 0", i, row.WallMS.Min)
		}
	}

	if len(report.Recovery) != 1 {
		t.Fatalf("recovery has %d rows, want one per lease TTL", len(report.Recovery))
	}
	if row := report.Recovery[0]; row.LeaseTTLMS != 1000 || row.DuplicateMutations != 0 || row.RecoveryMS.Median <= 0 {
		t.Errorf("recovery row is wrong: %+v", row)
	}

	if report.Replay == nil {
		t.Fatal("replay section is missing")
	}
	if report.Replay.Runs != 2 || report.Replay.Verified != 2 || report.Replay.MS.Min <= 0 {
		t.Errorf("replay row is wrong: %+v", *report.Replay)
	}

	if report.CrashMatrix == nil {
		t.Fatal("crash_matrix section is missing")
	}
	if m := *report.CrashMatrix; m.Scenario != "duplicate-charge" || m.Cases == 0 || m.Violations != 0 || m.WallMS <= 0 {
		t.Errorf("crash matrix row is wrong: %+v", m)
	}
	if problems := report.Problems(); len(problems) != 0 {
		t.Errorf("a healthy run reported problems: %v", problems)
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err = json.Unmarshal(encoded, &generic); err != nil {
		t.Fatal(err)
	}
	if keys := keysNamed(generic, "dsn"); len(keys) != 0 {
		t.Errorf("report JSON carries a dsn key: %v", keys)
	}
	for _, key := range []string{"schema_version", "environment", "backend", "config", "throughput", "recovery", "replay", "crash_matrix"} {
		if _, ok := generic[key]; !ok {
			t.Errorf("report JSON has no %q key", key)
		}
	}
	if keys := keysNamed(generic["config"], "examples"); len(keys) != 0 {
		t.Errorf("config JSON exposes the examples path: %v", keys)
	}
}

// environmentKeys runs the replay workload alone, which is the quickest, with
// the given build description, and returns the keys of the report's environment
// block as they are written to disk.
func environmentKeys(t *testing.T, buildJSON string) map[string]any {
	t.Helper()
	var build buildinfo.Info
	if err := json.Unmarshal([]byte(buildJSON), &build); err != nil {
		t.Fatalf("build info %s: %v", buildJSON, err)
	}
	report, err := Run(context.Background(), Config{
		Examples:   examplesRoot,
		Workloads:  []string{WorkloadReplay},
		ReplayRuns: 1,
		Build:      build,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var generic struct {
		Environment map[string]any `json:"environment"`
	}
	if err = json.Unmarshal(encoded, &generic); err != nil {
		t.Fatal(err)
	}
	return generic.Environment
}

// A build with no VCS settings, such as a go run binary, never read the tree. A
// report from it must not say the tree was clean, and it must not look as if it
// had a revision either.
func TestReportFromABuildWithoutVCSSettingsDoesNotAssertACleanTree(t *testing.T) {
	env := environmentKeys(t, `{"twinwright_version":"dev","go_version":"go1.27.3"}`)
	if value, present := env["git_modified"]; present {
		t.Errorf("git_modified = %v in a report whose build recorded no tree state: %v", value, env)
	}
	if value, present := env["git_revision"]; present {
		t.Errorf("git_revision = %v in a report whose build recorded none: %v", value, env)
	}
}

// A stamped build keeps both answers, including a recorded false: that is the
// statement the committed reports make.
func TestReportFromAStampedBuildRecordsTheTreeState(t *testing.T) {
	for _, modified := range []bool{false, true} {
		raw := `{"twinwright_version":"v0.1.2","git_revision":"abc123","git_modified":` + strconv.FormatBool(modified) + `,"go_version":"go1.27.3"}`
		env := environmentKeys(t, raw)
		if env["git_revision"] != "abc123" {
			t.Errorf("git_revision = %v, want abc123: %v", env["git_revision"], env)
		}
		if got, present := env["git_modified"]; !present || got != modified {
			t.Errorf("git_modified = %v (present %v), want the recorded %v", got, present, modified)
		}
	}
}

func TestPerfRecoveryIsBoundedByTheLease(t *testing.T) {
	report, err := Run(context.Background(), Config{
		Examples:  examplesRoot,
		Workloads: []string{WorkloadRecovery},
		LeaseTTLs: []time.Duration{time.Second},
		Trials:    3,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Throughput) != 0 || report.Replay != nil || report.CrashMatrix != nil {
		t.Errorf("only recovery was requested: %+v", report)
	}
	if len(report.Recovery) != 1 {
		t.Fatalf("recovery has %d rows, want 1", len(report.Recovery))
	}
	row := report.Recovery[0]
	// The lease is the floor: nobody may take the run over before it expires. The
	// ceiling is the lease plus a poll, the takeover and the rest of the run, and
	// three seconds leaves room for a loaded machine without hiding a worker that
	// never polls.
	if row.RecoveryMS.Median < 1000 || row.RecoveryMS.Median > 3000 {
		t.Errorf("recovery median %.1f ms with a 1000 ms lease, want between 1000 and 3000 (min %.1f, max %.1f)",
			row.RecoveryMS.Median, row.RecoveryMS.Min, row.RecoveryMS.Max)
	}
	if row.RecoveryMS.Min < 1000 {
		t.Errorf("a run was taken over %.1f ms after the crash, before its 1000 ms lease could expire", row.RecoveryMS.Min)
	}
	if row.DuplicateMutations != 0 {
		t.Errorf("duplicate_mutations=%d want 0", row.DuplicateMutations)
	}
}

func TestStatOf(t *testing.T) {
	tests := []struct {
		name   string
		values []float64
		want   Stat
	}{
		{"one value", []float64{4}, Stat{Min: 4, Median: 4, Max: 4}},
		{"odd count takes the middle value", []float64{9, 1, 5}, Stat{Min: 1, Median: 5, Max: 9}},
		{"even count averages the two middle values", []float64{8, 2, 4, 6}, Stat{Min: 2, Median: 5, Max: 8}},
		{"ties", []float64{3, 3, 3}, Stat{Min: 3, Median: 3, Max: 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := append([]float64(nil), tc.values...)
			if got := statOf(input); got != tc.want {
				t.Errorf("statOf(%v)=%+v want %+v", tc.values, got, tc.want)
			}
			for i := range input {
				if input[i] != tc.values[i] {
					t.Fatalf("statOf reordered its input: %v", input)
				}
			}
		})
	}
	if got := statOf(nil); got != (Stat{}) {
		t.Errorf("statOf(nil)=%+v want the zero Stat", got)
	}
}

func TestStatJSONNames(t *testing.T) {
	encoded, err := json.Marshal(Stat{Min: 1, Median: 2, Max: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(encoded), `{"min":1,"median":2,"max":3}`; got != want {
		t.Errorf("Stat JSON=%s want %s", got, want)
	}
}

func TestConfigResolveAppliesDefaults(t *testing.T) {
	cfg, err := Config{}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Workers; len(got) != 4 || got[0] != 1 || got[1] != 2 || got[2] != 4 || got[3] != 8 {
		t.Errorf("Workers=%v want 1,2,4,8", got)
	}
	if cfg.Runs != 40 || cfg.Trials != 3 || cfg.ReplayRuns != 20 || cfg.Seed != 42 {
		t.Errorf("scalar defaults are wrong: %+v", cfg)
	}
	if got := cfg.LeaseTTLs; len(got) != 3 || got[0] != time.Second || got[1] != 2*time.Second || got[2] != 5*time.Second {
		t.Errorf("LeaseTTLs=%v want 1s,2s,5s", got)
	}
	if got := strings.Join(cfg.Workloads, ","); got != "throughput,recovery,replay,crash-matrix" {
		t.Errorf("Workloads=%q want all four in execution order", got)
	}
	if cfg.Examples != "examples" {
		t.Errorf("Examples=%q want examples", cfg.Examples)
	}
}

func TestConfigResolveKeepsWorkloadsInExecutionOrder(t *testing.T) {
	cfg, err := Config{Workloads: []string{WorkloadCrashMatrix, WorkloadThroughput}}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Workloads, ","); got != "throughput,crash-matrix" {
		t.Errorf("Workloads=%q want execution order throughput,crash-matrix", got)
	}
}

func TestConfigResolveDoesNotAliasTheCallersSlices(t *testing.T) {
	workers := []int{3}
	cfg, err := Config{Workers: workers}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workers[0] = 99
	if workers[0] != 3 {
		t.Errorf("Resolve shares the caller's Workers slice: %v", workers)
	}
}

func TestConfigResolveRejectsBadValues(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"zero worker count", Config{Workers: []int{1, 0}}, "worker count"},
		{"negative worker count", Config{Workers: []int{-2}}, "worker count"},
		{"negative runs", Config{Runs: -1}, "runs"},
		{"negative trials", Config{Trials: -1}, "trials"},
		{"negative replay runs", Config{ReplayRuns: -1}, "replay"},
		{"zero lease ttl", Config{LeaseTTLs: []time.Duration{0}}, "lease ttl"},
		{"negative lease ttl", Config{LeaseTTLs: []time.Duration{-time.Second}}, "lease ttl"},
		{"lease ttl under the store minimum", Config{LeaseTTLs: []time.Duration{500 * time.Millisecond}}, "at least"},
		{"unknown workload", Config{Workloads: []string{"throughput", "latency"}}, "latency"},
		{"repeated workload", Config{Workloads: []string{"replay", "replay"}}, "twice"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.cfg.Resolve()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Resolve err=%v want an error mentioning %q", err, tc.want)
			}
		})
	}
}

func TestConfigJSONUsesMillisecondsAndOmitsPrivateFields(t *testing.T) {
	const dsn = "postgres://perf:hunter2-secret@db.internal:5432/perf?sslmode=disable"
	cfg, err := Config{
		DSN:       dsn,
		Examples:  "/srv/examples",
		Workers:   []int{1, 2},
		LeaseTTLs: []time.Duration{1500 * time.Millisecond, 2 * time.Second},
	}.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, leaked := range []string{"hunter2", "db.internal", "/srv/examples", "dsn", "examples"} {
		if strings.Contains(strings.ToLower(text), leaked) {
			t.Errorf("config JSON leaks %q: %s", leaked, text)
		}
	}
	var generic map[string]any
	if err = json.Unmarshal(encoded, &generic); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"workers", "runs", "trials", "lease_ttls_ms", "replay_runs", "seed", "workloads"} {
		if _, ok := generic[key]; !ok {
			t.Errorf("config JSON has no %q key: %s", key, text)
		}
	}
	if got := generic["lease_ttls_ms"]; len(got.([]any)) != 2 || got.([]any)[0] != float64(1500) || got.([]any)[1] != float64(2000) {
		t.Errorf("lease_ttls_ms=%v want [1500 2000]", got)
	}

	var decoded Config
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.LeaseTTLs) != 2 || decoded.LeaseTTLs[0] != 1500*time.Millisecond || decoded.LeaseTTLs[1] != 2*time.Second {
		t.Errorf("round trip lost the lease TTLs: %v", decoded.LeaseTTLs)
	}
	if decoded.DSN != "" || decoded.Examples != "" {
		t.Errorf("round trip invented private fields: %+v", decoded)
	}
}

// malformedDSNs are DSNs whose password holds a reserved character that was not
// percent-encoded, so url.Parse rejects them and the error it builds quotes the
// part of the string it choked on. Each one makes the underlying error carry the
// password, which is the only input on which the scrubber has anything to do:
// pgx's own connection errors never contain it.
var malformedDSNs = map[string]struct{ dsn, password string }{
	"slash":             {"postgres://perf:hunter2/secret@127.0.0.1:1/perf?sslmode=disable", "hunter2/secret"},
	"hash":              {"postgres://perf:hunter2#secret@127.0.0.1:1/perf?sslmode=disable", "hunter2#secret"},
	"question mark":     {"postgres://perf:hunter2?secret@127.0.0.1:1/perf?sslmode=disable", "hunter2?secret"},
	"bad escape":        {"postgres://perf:hun%zzer2-secret@127.0.0.1:1/perf?sslmode=disable", "hun%zzer2-secret"},
	"digits then slash": {"postgres://perf:12345/secret@127.0.0.1:1/perf?sslmode=disable", "12345/secret"},
	"digits then hash":  {"postgres://perf:12345#secret@127.0.0.1:1/perf?sslmode=disable", "12345#secret"},
}

// assertNoPasswordIn fails when text carries the DSN, the password, or any four
// character piece of the password: a scrubber that removes the whole password and
// leaves its first half has not scrubbed anything.
func assertNoPasswordIn(t *testing.T, text, dsn, password string) {
	t.Helper()
	if strings.Contains(text, strings.TrimSpace(dsn)) {
		t.Errorf("the DSN itself is in the text: %s", text)
	}
	for i := 0; i+4 <= len(password); i++ {
		if piece := password[i : i+4]; strings.Contains(text, piece) {
			t.Errorf("a piece of the password (%q) is in the text: %s", piece, text)
			return
		}
	}
}

func TestScrubErrorRemovesTheDSN(t *testing.T) {
	const dsn = "postgres://perf:hunter2-secret@db.internal:5432/perf?sslmode=disable&password=also-secret"
	cause := errors.New("connect to " + dsn + ": refused for password hunter2-secret")
	scrubbed := scrubError(cause, dsn)
	if scrubbed == nil {
		t.Fatal("scrubError dropped the error")
	}
	for _, leaked := range []string{"hunter2-secret", "also-secret", "perf:hunter2", dsn} {
		if strings.Contains(scrubbed.Error(), leaked) {
			t.Errorf("scrubbed error still carries %q: %s", leaked, scrubbed)
		}
	}
	if !errors.Is(scrubbed, cause) {
		t.Error("scrubbing hid the cause from errors.Is")
	}
	if scrubError(nil, dsn) != nil {
		t.Error("scrubError(nil) must stay nil")
	}
	plain := errors.New("no database involved")
	if got := scrubError(plain, ""); got != plain {
		t.Errorf("an error with no DSN to hide was rewritten: %v", got)
	}
}

// url.Parse quotes what it rejects, so an error about a DSN that does not parse
// carries the password, in a form (a piece of it, in a message about a bad port)
// that no replacement of the whole DSN can find. The scrubber therefore drops the
// cause's text for such a DSN and says only what is wrong.
func TestScrubErrorDropsTheTextOfAnErrorAboutAMalformedDSN(t *testing.T) {
	for name, tc := range malformedDSNs {
		_, parseErr := url.Parse(tc.dsn)
		if parseErr == nil {
			// These parse, with the front of the password as the port. The error
			// pgx builds for them is not url.Parse's, so Run's test covers them.
			continue
		}
		t.Run(name, func(t *testing.T) {
			// The shape the store gives it: its own prefix around url.Parse's text.
			cause := errors.New("parse postgres DSN: " + parseErr.Error())
			scrubbed := scrubError(cause, tc.dsn)
			assertNoPasswordIn(t, scrubbed.Error(), tc.dsn, tc.password)
			if !errors.Is(scrubbed, cause) {
				t.Error("scrubbing hid the cause from errors.Is")
			}
			if !strings.Contains(scrubbed.Error(), "--db") {
				t.Errorf("the scrubbed error does not say which flag is wrong: %s", scrubbed)
			}
		})
	}
}

// Run's error is what the command prints, so this is the path that matters. The
// DSN must be one whose unscrubbed error carries the password; with the scrubber
// removed, every case here fails.
func TestRunNeverPrintsAMalformedDSN(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for name, tc := range malformedDSNs {
		t.Run(name, func(t *testing.T) {
			_, err := Run(ctx, Config{DSN: tc.dsn, Examples: examplesRoot, Workloads: []string{WorkloadReplay}, ReplayRuns: 1})
			if err == nil {
				t.Fatal("Run succeeded against a DSN that cannot be parsed")
			}
			assertNoPasswordIn(t, err.Error(), tc.dsn, tc.password)
		})
	}
}

func TestRunNeverPrintsTheDSNOnAConnectionFailure(t *testing.T) {
	const dsn = "postgres://perf:hunter2-secret@127.0.0.1:1/perf?sslmode=disable"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := Run(ctx, Config{DSN: dsn, Examples: examplesRoot, Workloads: []string{WorkloadReplay}, ReplayRuns: 1})
	if err == nil {
		t.Fatal("Run succeeded against a port nothing listens on")
	}
	if strings.Contains(err.Error(), "hunter2-secret") || strings.Contains(err.Error(), dsn) {
		t.Errorf("error text carries the DSN: %v", err)
	}
}

func TestRequireIdleQueue(t *testing.T) {
	ctx := context.Background()
	sc := duplicateCharge(t)
	s := openSQLite(t)
	if err := requireIdleQueue(ctx, s); err != nil {
		t.Fatalf("an empty queue is idle: %v", err)
	}

	run, err := sc.Seed(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Enqueue(ctx, run.ID, sc.Target.Steps, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	err = requireIdleQueue(ctx, s)
	if err == nil || !strings.Contains(err.Error(), "1 runnable") {
		t.Fatalf("a runnable entry must be refused with its count: %v", err)
	}

	if _, _, err = s.ClaimRun(ctx, "someone-else", time.Minute, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	err = requireIdleQueue(ctx, s)
	if err == nil || !strings.Contains(err.Error(), "1 leased") {
		t.Fatalf("a leased entry must be refused with its count: %v", err)
	}
}

// fleetExecute runs a claimed run the way the harness does, for tests that need
// a real execution without going through Run.
func fleetExecute(s *store.Store, sc crashmatrix.Scenario) worker.ExecuteFunc {
	h := &harness{sc: sc}
	return h.executor(s)
}

func seedQueued(t *testing.T, s *store.Store, sc crashmatrix.Scenario, n int) {
	t.Helper()
	ctx := context.Background()
	for range n {
		run, err := sc.Seed(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Enqueue(ctx, run.ID, sc.Target.Steps, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunFleetCompletesEveryQueuedRun(t *testing.T) {
	sc := duplicateCharge(t)
	s := openSQLite(t)
	seedQueued(t, s, sc, 6)
	wall, err := runFleet(context.Background(), s, 3, 6, fleetExecute(s, sc))
	if err != nil {
		t.Fatalf("runFleet: %v", err)
	}
	if wall <= 0 {
		t.Errorf("wall=%v want > 0", wall)
	}
	depth, err := s.QueueDepth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if depth[store.QueueFinished] != 6 {
		t.Errorf("queue depth %v want 6 finished", depth)
	}
}

func TestRunFleetFailsWhenARunDoesNotComplete(t *testing.T) {
	sc := duplicateCharge(t)
	s := openSQLite(t)
	seedQueued(t, s, sc, 2)
	failing := func(context.Context, store.RunLease, store.QueueEntry) (store.Run, error) {
		return store.Run{}, errors.New("boom")
	}
	_, err := runFleet(context.Background(), s, 2, 2, failing)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("a run that failed must fail the trial, naming why: %v", err)
	}
}

func TestRunFleetFailsWhenFewerRunsCompleteThanWereQueued(t *testing.T) {
	sc := duplicateCharge(t)
	s := openSQLite(t)
	seedQueued(t, s, sc, 2)
	_, err := runFleet(context.Background(), s, 1, 3, fleetExecute(s, sc))
	if err == nil || !strings.Contains(err.Error(), "2 of 3") {
		t.Fatalf("a short drain must fail the trial with the counts: %v", err)
	}
}

func TestDuplicateRefundsSeesAnExtraEffect(t *testing.T) {
	ctx := context.Background()
	sc := duplicateCharge(t)
	s := openSQLite(t)
	h := &harness{sc: sc}

	run, err := sc.Seed(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = duplicateRefunds(ctx, s, run.WorldID); err == nil {
		t.Error("a world with no refund is a broken measurement, not zero duplicates")
	}

	done, err := h.localRunner(s).Execute(ctx, run.ID, sc.Target.Steps)
	if err != nil || done.Status != "completed" {
		t.Fatalf("run: status=%q err=%v", done.Status, err)
	}
	if got, err := duplicateRefunds(ctx, s, run.WorldID); err != nil || got != 0 {
		t.Fatalf("one refund is zero duplicates: got %d, %v", got, err)
	}

	d := &dispatch.Dispatcher{Store: s, Manifest: sc.Target.Manifest}
	extra := map[string]any{"charge_id": "CH-1001", "amount_cents": 100, "reason": "extra refund"}
	if result, err := d.Invoke(ctx, run.ID, "perf-extra", "createRefund", extra); err != nil || result.Status != 201 {
		t.Fatalf("extra refund: status=%d err=%v", result.Status, err)
	}
	if got, err := duplicateRefunds(ctx, s, run.WorldID); err != nil || got != 1 {
		t.Fatalf("a second refund is one duplicate: got %d, %v", got, err)
	}
}

func TestProblems(t *testing.T) {
	clean := Report{
		Recovery:    []RecoveryResult{{LeaseTTLMS: 1000}},
		CrashMatrix: &MatrixResult{Scenario: "duplicate-charge", Cases: 10},
	}
	if got := clean.Problems(); len(got) != 0 {
		t.Errorf("clean report has problems: %v", got)
	}

	duplicated := Report{Recovery: []RecoveryResult{{LeaseTTLMS: 1000}, {LeaseTTLMS: 2000, DuplicateMutations: 2}}}
	got := duplicated.Problems()
	if len(got) != 1 || !strings.Contains(got[0], "2000") || !strings.Contains(got[0], "2") {
		t.Errorf("a duplicated mutation must name the TTL and the count: %v", got)
	}

	violated := Report{CrashMatrix: &MatrixResult{Scenario: "duplicate-charge", Cases: 10, Violations: 3}}
	got = violated.Problems()
	if len(got) != 1 || !strings.Contains(got[0], "3") {
		t.Errorf("crash matrix violations must be named with their count: %v", got)
	}

	both := Report{
		Recovery:    duplicated.Recovery,
		CrashMatrix: violated.CrashMatrix,
	}
	if got := both.Problems(); len(got) != 2 {
		t.Errorf("want one problem per finding, got %v", got)
	}

	// Replay verification has a right answer, like the other two: a run the ledger
	// cannot reproduce is a finding, not a slower figure.
	allVerified := Report{Replay: &ReplayResult{Runs: 2, Verified: 2}}
	if got := allVerified.Problems(); len(got) != 0 {
		t.Errorf("a replay that verified every run is not a problem: %v", got)
	}
	unverified := Report{Replay: &ReplayResult{Runs: 2, Verified: 1}}
	got = unverified.Problems()
	if len(got) != 1 || !strings.Contains(got[0], "replay") || !strings.Contains(got[0], "1 of 2") {
		t.Errorf("an unverified replay must be named with its counts: %v", got)
	}
}

func TestFormatTextSummarisesEverySection(t *testing.T) {
	report := Report{
		SchemaVersion: 1,
		Backend:       "sqlite",
		Throughput: []ThroughputResult{{Workers: 2, Runs: 8,
			RunsPerSecond: Stat{Min: 10, Median: 12, Max: 14}, WallMS: Stat{Min: 500, Median: 600, Max: 700}}},
		Recovery:    []RecoveryResult{{LeaseTTLMS: 1000, RecoveryMS: Stat{Min: 1010, Median: 1050, Max: 1100}}},
		Replay:      &ReplayResult{Runs: 5, Verified: 5, MS: Stat{Min: 1, Median: 2, Max: 3}},
		CrashMatrix: &MatrixResult{Scenario: "duplicate-charge", Cases: 27, Violations: 0, WallMS: 900},
	}
	report.Environment.GOOS = "linux"
	report.Environment.GOARCH = "amd64"
	text := report.FormatText()
	for _, want := range []string{"sqlite", "throughput", "recovery", "replay", "crash matrix", "duplicate-charge", "27 cases"} {
		if !strings.Contains(strings.ToLower(text), want) {
			t.Errorf("summary does not mention %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\u2014") {
		t.Errorf("summary contains an em dash:\n%s", text)
	}
}

// TestPerfPostgres runs a small configuration against a real server, where the
// throughput trials share one database and the crash matrix runs its cases on
// it in turn. It is skipped without TWINWRIGHT_TEST_POSTGRES_DSN.
func TestPerfPostgres(t *testing.T) {
	dsn := pgtest.SchemaDSN(t)
	report, err := Run(context.Background(), Config{
		DSN:        dsn,
		Examples:   examplesRoot,
		Workers:    []int{2},
		Runs:       4,
		Trials:     1,
		LeaseTTLs:  []time.Duration{time.Second},
		ReplayRuns: 2,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Backend != "postgres" {
		t.Errorf("backend=%q want postgres", report.Backend)
	}
	if problems := report.Problems(); len(problems) != 0 {
		t.Errorf("problems on postgres: %v", problems)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), dsn) || strings.Contains(string(encoded), "search_path") {
		t.Errorf("report carries the DSN: %s", encoded)
	}
}

func TestRunRemovesItsTemporaryFiles(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	_, err := Run(context.Background(), Config{
		Examples:   examplesRoot,
		Workers:    []int{1},
		Runs:       1,
		Trials:     1,
		ReplayRuns: 1,
		Workloads:  []string{WorkloadThroughput, WorkloadReplay},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("Run left %v behind", names)
	}
}
