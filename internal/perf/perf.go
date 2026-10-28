// Package perf measures the platform's workloads and writes the figures to a
// report that says which machine and build produced them.
//
// Four workloads run the same scripted duplicate-charge scenario the crash
// matrix and the benchmark suite run, so a figure here describes the code a test
// already proves correct: how fast a fleet of workers drains a queue, how long a
// crashed worker's run waits before another worker finishes it, how long
// replaying a finished run takes, and how long the whole crash matrix takes.
//
// The report exists so that no number in the documentation is typed by hand. A
// figure is quoted from a report, with the environment block that came with it,
// or it is not quoted. Nothing here claims a number is a property of the
// project: each is one machine's measurement on one backend.
package perf

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"time"

	"twinwright/internal/bench"
	"twinwright/internal/buildinfo"
	"twinwright/internal/redact"
	"twinwright/internal/store"
)

// SchemaVersion is the report layout this build writes.
const SchemaVersion = 1

// Workload names, in the order Run executes them.
const (
	WorkloadThroughput  = "throughput"
	WorkloadRecovery    = "recovery"
	WorkloadReplay      = "replay"
	WorkloadCrashMatrix = "crash-matrix"
)

// AllWorkloads returns every workload name in execution order.
func AllWorkloads() []string {
	return []string{WorkloadThroughput, WorkloadRecovery, WorkloadReplay, WorkloadCrashMatrix}
}

// Defaults applied by Config.Resolve for a field left at its zero value.
const (
	DefaultRuns       = 40
	DefaultTrials     = 3
	DefaultReplayRuns = 20
	DefaultSeed       = 42
	DefaultExamples   = "examples"
)

// minLeaseTTL is the shortest lease the store accepts. Checking it up front
// turns a mistyped 500ms into an input error instead of a failure halfway
// through a measurement.
const minLeaseTTL = time.Second

// Config selects what to measure. A zero field takes its default, so the zero
// Config measures everything on a temporary SQLite file.
type Config struct {
	// DSN is a SQLite file path or a postgres:// URL. Empty means a temporary
	// SQLite file that Run creates and removes. It never appears in a report or
	// in an error Run returns, because a DSN can carry a password.
	//
	// The recovery and replay workloads run on it. On SQLite, each throughput
	// trial and each crash matrix case still gets a temporary file of its own, so
	// no trial inherits another's rows; on PostgreSQL they share the database and
	// each starts only when its work queue is idle.
	DSN string `json:"-"`
	// Examples is the examples directory the scenario is compiled from.
	Examples string `json:"-"`
	// Build identifies the binary doing the measuring and is recorded in the
	// report's environment. The zero value is read from the running binary,
	// which is right for tests and wrong only in that it lacks the linker's
	// version string, so the command supplies it.
	Build buildinfo.Info `json:"-"`

	// Workers lists the fleet sizes the throughput workload tries.
	Workers []int
	// Runs is how many runs one throughput trial drains.
	Runs int
	// Trials is how many times each throughput and recovery measurement repeats.
	Trials int
	// LeaseTTLs lists the lease lifetimes the recovery workload tries.
	LeaseTTLs []time.Duration
	// ReplayRuns is how many finished runs the replay workload verifies.
	ReplayRuns int
	// Seed is the world seed of every run perf seeds itself. The crash matrix
	// workload keeps crashmatrix's own fixed seed (42) whatever this is, so the
	// matrix measured here is the one the test suite runs.
	Seed int64
	// Workloads is a subset of the four workload names.
	Workloads []string
}

// DefaultConfig is the configuration the command's flags default to.
func DefaultConfig() Config {
	cfg, _ := Config{}.Resolve()
	return cfg
}

// Resolve applies defaults and rejects values no measurement could use. It
// returns a copy, so the caller's slices are never aliased or reordered. A
// zero scalar means "use the default"; a negative one is an error, which is why
// the command's flags refuse zero themselves instead of passing it through.
func (c Config) Resolve() (Config, error) {
	c.Workers = slices.Clone(c.Workers)
	c.LeaseTTLs = slices.Clone(c.LeaseTTLs)
	c.Workloads = slices.Clone(c.Workloads)

	if len(c.Workers) == 0 {
		c.Workers = []int{1, 2, 4, 8}
	}
	for _, workers := range c.Workers {
		if workers < 1 {
			return Config{}, fmt.Errorf("worker count %d must be positive", workers)
		}
	}
	if c.Runs < 0 {
		return Config{}, fmt.Errorf("runs must be positive, got %d", c.Runs)
	}
	if c.Runs == 0 {
		c.Runs = DefaultRuns
	}
	if c.Trials < 0 {
		return Config{}, fmt.Errorf("trials must be positive, got %d", c.Trials)
	}
	if c.Trials == 0 {
		c.Trials = DefaultTrials
	}
	if c.ReplayRuns < 0 {
		return Config{}, fmt.Errorf("replay runs must be positive, got %d", c.ReplayRuns)
	}
	if c.ReplayRuns == 0 {
		c.ReplayRuns = DefaultReplayRuns
	}
	if len(c.LeaseTTLs) == 0 {
		c.LeaseTTLs = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second}
	}
	for _, ttl := range c.LeaseTTLs {
		switch {
		case ttl <= 0:
			return Config{}, fmt.Errorf("lease ttl %s must be positive", ttl)
		case ttl < minLeaseTTL:
			return Config{}, fmt.Errorf("lease ttl %s must be at least %s", ttl, minLeaseTTL)
		}
	}
	if c.Seed == 0 {
		c.Seed = DefaultSeed
	}
	if c.Examples == "" {
		c.Examples = DefaultExamples
	}
	workloads, err := resolveWorkloads(c.Workloads)
	if err != nil {
		return Config{}, err
	}
	c.Workloads = workloads
	return c, nil
}

// resolveWorkloads validates the requested names and returns them in execution
// order, so the report does not depend on how the caller happened to list them.
func resolveWorkloads(requested []string) ([]string, error) {
	all := AllWorkloads()
	if len(requested) == 0 {
		return all, nil
	}
	wanted := map[string]bool{}
	for _, name := range requested {
		if !slices.Contains(all, name) {
			return nil, fmt.Errorf("unknown workload %q; valid workloads: %s", name, strings.Join(all, ", "))
		}
		if wanted[name] {
			return nil, fmt.Errorf("workload %q listed twice", name)
		}
		wanted[name] = true
	}
	ordered := make([]string, 0, len(wanted))
	for _, name := range all {
		if wanted[name] {
			ordered = append(ordered, name)
		}
	}
	return ordered, nil
}

// configJSON is Config's wire form: durations as integer milliseconds, which
// is how the report states every other time.
type configJSON struct {
	Workers     []int    `json:"workers"`
	Runs        int      `json:"runs"`
	Trials      int      `json:"trials"`
	LeaseTTLsMS []int64  `json:"lease_ttls_ms"`
	ReplayRuns  int      `json:"replay_runs"`
	Seed        int64    `json:"seed"`
	Workloads   []string `json:"workloads"`
}

// MarshalJSON writes the measurement settings only. DSN, Examples and Build are
// inputs to a run, not part of what it measured, and the DSN can hold a secret.
func (c Config) MarshalJSON() ([]byte, error) {
	ttls := make([]int64, len(c.LeaseTTLs))
	for i, ttl := range c.LeaseTTLs {
		ttls[i] = ttl.Milliseconds()
	}
	return json.Marshal(configJSON{
		Workers: c.Workers, Runs: c.Runs, Trials: c.Trials, LeaseTTLsMS: ttls,
		ReplayRuns: c.ReplayRuns, Seed: c.Seed, Workloads: c.Workloads,
	})
}

// UnmarshalJSON reads what MarshalJSON wrote, so a report can be decoded.
func (c *Config) UnmarshalJSON(data []byte) error {
	var wire configJSON
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	c.Workers, c.Runs, c.Trials = wire.Workers, wire.Runs, wire.Trials
	c.ReplayRuns, c.Seed, c.Workloads = wire.ReplayRuns, wire.Seed, wire.Workloads
	c.LeaseTTLs = make([]time.Duration, len(wire.LeaseTTLsMS))
	for i, ms := range wire.LeaseTTLsMS {
		c.LeaseTTLs[i] = time.Duration(ms) * time.Millisecond
	}
	return nil
}

// Stat summarises one measurement over its trials. The median of an even count
// is the mean of the two middle values.
type Stat struct {
	Min    float64 `json:"min"`
	Median float64 `json:"median"`
	Max    float64 `json:"max"`
}

// statOf summarises values without reordering the slice it was given. Results
// are rounded to three decimals: finer digits are timer noise, and they make a
// report harder to read and to diff.
func statOf(values []float64) Stat {
	if len(values) == 0 {
		return Stat{}
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	n := len(sorted)
	median := sorted[n/2]
	if n%2 == 0 {
		median = (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return Stat{Min: round3(sorted[0]), Median: round3(median), Max: round3(sorted[n-1])}
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// millis converts a duration to fractional milliseconds.
func millis(d time.Duration) float64 { return round3(float64(d) / float64(time.Millisecond)) }

// ThroughputResult is one fleet size's drain rate over its trials.
type ThroughputResult struct {
	Workers       int  `json:"workers"`
	Runs          int  `json:"runs"`
	RunsPerSecond Stat `json:"runs_per_second"`
	WallMS        Stat `json:"wall_ms"`
}

// RecoveryResult is one lease TTL's crash recovery time over its trials.
//
// RecoveryMS runs from the moment the first worker has died, with its lease
// renewed at that instant, to the moment a second worker reports the run
// finished. A worker that dies right after a renewal is the longest wait a
// lease allows, so this is the bound, not the average: a worker that died
// earlier in its renewal period would be recovered sooner.
type RecoveryResult struct {
	LeaseTTLMS int64 `json:"lease_ttl_ms"`
	RecoveryMS Stat  `json:"recovery_ms"`
	// DuplicateMutations counts committed refunds beyond the one the run owns,
	// summed over the trials. Delivery is at-least-once and the work is safe to
	// repeat, so this must be zero.
	DuplicateMutations int `json:"duplicate_mutations"`
}

// ReplayResult is the replay workload's outcome. MS is over the per-run
// verification times, not over trials.
type ReplayResult struct {
	Runs     int  `json:"runs"`
	Verified int  `json:"verified"`
	MS       Stat `json:"ms"`
}

// MatrixResult is the crash matrix workload's outcome for one scenario.
type MatrixResult struct {
	Scenario   string  `json:"scenario"`
	Cases      int     `json:"cases"`
	Violations int     `json:"violations"`
	WallMS     float64 `json:"wall_ms"`
}

// Report is the machine-readable output of one perf run. Backend is "sqlite" or
// "postgres" and is never the connection string.
type Report struct {
	SchemaVersion int                `json:"schema_version"`
	Environment   bench.Environment  `json:"environment"`
	Backend       string             `json:"backend"`
	Config        Config             `json:"config"`
	Throughput    []ThroughputResult `json:"throughput,omitempty"`
	Recovery      []RecoveryResult   `json:"recovery,omitempty"`
	Replay        *ReplayResult      `json:"replay,omitempty"`
	CrashMatrix   *MatrixResult      `json:"crash_matrix,omitempty"`
}

// Problems lists the findings that should fail a run: a recovery that repeated
// a committed effect, and a crash matrix that reported a violation. An empty
// list means nothing the report measured broke an invariant. It does not judge
// the speed figures, which have no pass mark.
func (r Report) Problems() []string {
	var problems []string
	for _, row := range r.Recovery {
		if row.DuplicateMutations > 0 {
			problems = append(problems, fmt.Sprintf(
				"recovery with a %d ms lease duplicated %d committed mutation(s)", row.LeaseTTLMS, row.DuplicateMutations))
		}
	}
	if m := r.CrashMatrix; m != nil && m.Violations > 0 {
		problems = append(problems, fmt.Sprintf(
			"crash matrix %s reported %d violation(s) across %d case(s)", m.Scenario, m.Violations, m.Cases))
	}
	return problems
}

// FormatText is the short summary the command prints. The JSON report is the
// record; this is for a person watching the run.
func (r Report) FormatText() string {
	var b strings.Builder
	e := r.Environment
	fmt.Fprintf(&b, "twinwright perf: backend %s, %s/%s, %d CPUs, %s, seed %d\n",
		r.Backend, e.GOOS, e.GOARCH, e.NumCPU, e.GoVersion, e.Seed)
	if len(r.Throughput) > 0 {
		fmt.Fprintf(&b, "throughput (%d runs per trial, %d trials):\n", r.Throughput[0].Runs, r.Config.Trials)
		for _, row := range r.Throughput {
			fmt.Fprintf(&b, "  %d worker(s): %.1f runs/s median (min %.1f, max %.1f)\n",
				row.Workers, row.RunsPerSecond.Median, row.RunsPerSecond.Min, row.RunsPerSecond.Max)
		}
	}
	if len(r.Recovery) > 0 {
		fmt.Fprintf(&b, "recovery (%d trials):\n", r.Config.Trials)
		for _, row := range r.Recovery {
			fmt.Fprintf(&b, "  lease %d ms: %.1f ms median (min %.1f, max %.1f), %d duplicate mutation(s)\n",
				row.LeaseTTLMS, row.RecoveryMS.Median, row.RecoveryMS.Min, row.RecoveryMS.Max, row.DuplicateMutations)
		}
	}
	if row := r.Replay; row != nil {
		fmt.Fprintf(&b, "replay: %d of %d run(s) verified, %.1f ms median per run (min %.1f, max %.1f)\n",
			row.Verified, row.Runs, row.MS.Median, row.MS.Min, row.MS.Max)
	}
	if m := r.CrashMatrix; m != nil {
		fmt.Fprintf(&b, "crash matrix %s: %d cases, %d violation(s), %.1f ms\n", m.Scenario, m.Cases, m.Violations, m.WallMS)
	}
	return b.String()
}

// Run measures what cfg selects. An error means a measurement could not be made
// at all; a finding such as a violation is reported on the Report instead, so
// the caller can still write it down.
func Run(ctx context.Context, cfg Config) (Report, error) {
	report, err := run(ctx, cfg)
	if err != nil {
		return Report{}, scrubError(err, cfg.DSN)
	}
	return report, nil
}

func run(ctx context.Context, cfg Config) (Report, error) {
	cfg, err := cfg.Resolve()
	if err != nil {
		return Report{}, err
	}
	started := time.Now().UTC()
	build := cfg.Build
	if build.Version == "" {
		build = buildinfo.Read("")
	}

	h, err := newHarness(ctx, cfg)
	if err != nil {
		return Report{}, err
	}
	defer h.close()

	backend := "sqlite"
	if h.postgres {
		backend = "postgres"
	}
	// The report's copy of the config drops the DSN, so a Report printed with %+v
	// or logged whole cannot leak it either.
	reported := cfg
	reported.DSN = ""
	report := Report{
		SchemaVersion: SchemaVersion,
		Environment: bench.Environment{
			Info:      build,
			GOOS:      runtime.GOOS,
			GOARCH:    runtime.GOARCH,
			NumCPU:    runtime.NumCPU(),
			Seed:      cfg.Seed,
			StartedAt: started.Format(time.RFC3339),
		},
		Backend: backend,
		Config:  reported,
	}

	for _, name := range cfg.Workloads {
		switch name {
		case WorkloadThroughput:
			for _, workers := range cfg.Workers {
				row, err := h.throughput(ctx, workers)
				if err != nil {
					return Report{}, err
				}
				report.Throughput = append(report.Throughput, row)
			}
		case WorkloadRecovery:
			for _, ttl := range cfg.LeaseTTLs {
				row, err := h.recovery(ctx, ttl)
				if err != nil {
					return Report{}, err
				}
				report.Recovery = append(report.Recovery, row)
			}
		case WorkloadReplay:
			row, err := h.replay(ctx)
			if err != nil {
				return Report{}, err
			}
			report.Replay = &row
		case WorkloadCrashMatrix:
			row, err := h.crashMatrix(ctx)
			if err != nil {
				return Report{}, err
			}
			report.CrashMatrix = &row
		}
	}
	return report, nil
}

// scrubbedError is an error whose text has had a connection string removed. It
// still unwraps to the original, so errors.Is and errors.As see the cause.
type scrubbedError struct {
	text string
	err  error
}

func (e *scrubbedError) Error() string { return e.text }
func (e *scrubbedError) Unwrap() error { return e.err }

// scrubError removes dsn, and the secrets inside it, from err's text. A driver
// error can quote the connection string it failed to use, and the command
// prints whatever Run returns.
func scrubError(err error, dsn string) error {
	if err == nil || dsn == "" {
		return err
	}
	// A PostgreSQL URL that does not parse cleanly is the case replacement cannot
	// fix. url.Parse quotes what it rejected, and for a password with an
	// unencoded "/", "#" or "?" that is a piece of the password in a message about
	// a bad port, which no search for the whole DSN or the parsed password will
	// find. pgx builds the same kind of message for the ones that parse. So the
	// text is not cleaned, it is replaced, and the cause stays reachable for
	// errors.Is and errors.As.
	if store.IsPostgresDSN(dsn) && !redact.WellFormedURL(dsn) {
		return &scrubbedError{
			text: fmt.Sprintf("the --db value is not a usable postgres URL (%s): percent-encode any %%, /, # or ? in the password and remove spaces and control characters", redact.URL(dsn)),
			err:  err,
		}
	}
	text := err.Error()
	clean := strings.ReplaceAll(text, dsn, redact.URL(dsn))
	for _, secret := range dsnSecrets(dsn) {
		clean = strings.ReplaceAll(clean, secret, "[redacted]")
	}
	if clean == text {
		return err
	}
	return &scrubbedError{text: clean, err: err}
}

// dsnSecrets returns the parts of a URL-form DSN that must not be printed on
// their own: the password and any query value whose key suggests a credential.
// A DSN that does not parse as a URL has no parts to find; scrubError replaces
// the whole text of an error about one instead.
func dsnSecrets(dsn string) []string {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return nil
	}
	var secrets []string
	if parsed.User != nil {
		if password, ok := parsed.User.Password(); ok && password != "" {
			secrets = append(secrets, password)
		}
	}
	for key, values := range parsed.Query() {
		lower := strings.ToLower(key)
		if !strings.Contains(lower, "pass") && !strings.Contains(lower, "secret") &&
			!strings.Contains(lower, "token") && !strings.Contains(lower, "key") {
			continue
		}
		for _, value := range values {
			if value != "" {
				secrets = append(secrets, value)
			}
		}
	}
	return secrets
}
