package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"twinwright/internal/buildinfo"
	"twinwright/internal/perf"
)

// perfCommand measures the platform's workloads and writes the figures, with
// the environment that produced them, to a JSON report.
//
// Input mistakes are exit 2, as in gate, so a script can tell "fix the command
// line" from "the measurement found a problem" (exit 1). The report is written
// before the exit code is decided, so a run that found a duplicated mutation or
// a crash matrix violation leaves the evidence behind.
func perfCommand(ctx context.Context, args []string, out io.Writer) error {
	defaults := perf.DefaultConfig()
	fs := flag.NewFlagSet("perf", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	db := fs.String("db", "", "SQLite path or postgres:// DSN; empty uses a temporary SQLite file")
	examples := fs.String("examples", perf.DefaultExamples, "examples directory")
	workers := fs.String("workers", joinInts(defaults.Workers), "comma-separated fleet sizes for the throughput workload")
	runs := fs.Int("runs", defaults.Runs, "runs drained by each throughput trial")
	trials := fs.Int("trials", defaults.Trials, "times each throughput and recovery measurement repeats")
	leaseTTLs := fs.String("lease-ttls", joinDurations(defaults.LeaseTTLs), "comma-separated lease TTLs for the recovery workload, each at least 1s")
	replayRuns := fs.Int("replay-runs", defaults.ReplayRuns, "finished runs the replay workload verifies")
	seed := fs.Int64("seed", defaults.Seed, "world seed for the runs perf seeds itself (not zero); the crash matrix keeps its own fixed seed")
	workloads := fs.String("workloads", strings.Join(defaults.Workloads, ","), "comma-separated subset of "+strings.Join(perf.AllWorkloads(), ", "))
	outPath := fs.String("out", "", "write the JSON report to this path (required)")
	const usage = "usage: twinwright perf [--db DSN] [--examples DIR] [--workers 1,2,4,8] [--runs N] [--trials N] [--lease-ttls 1s,2s,5s] [--replay-runs N] [--seed N] [--workloads throughput,recovery,replay,crash-matrix] --out perf.json"
	if err := fs.Parse(args); err != nil {
		return badInput(fmt.Errorf("%s: %w", usage, err))
	}
	if fs.NArg() > 0 {
		return badInput(fmt.Errorf("perf takes no positional arguments, got %q", fs.Arg(0)))
	}
	if *outPath == "" {
		return badInput(fmt.Errorf("perf requires --out; %s", usage))
	}

	cfg := perf.Config{DSN: *db, Examples: *examples, Runs: *runs, Trials: *trials, ReplayRuns: *replayRuns, Seed: *seed}
	var err error
	if cfg.Workers, err = parseWorkerCounts(*workers); err != nil {
		return badInput(err)
	}
	if cfg.LeaseTTLs, err = parseLeaseTTLs(*leaseTTLs); err != nil {
		return badInput(err)
	}
	if cfg.Workloads, err = splitList("--workloads", *workloads); err != nil {
		return badInput(err)
	}
	// Zero would silently become the default inside Resolve. A flag that asks for
	// zero of something is a mistake to report, not a request for the default.
	for _, check := range []struct {
		name  string
		value int
	}{{"--runs", *runs}, {"--trials", *trials}, {"--replay-runs", *replayRuns}} {
		if check.value < 1 {
			return badInput(fmt.Errorf("%s must be positive, got %d", check.name, check.value))
		}
	}
	if *seed == 0 {
		return badInput(errors.New("--seed must not be zero; omit it to use the default"))
	}
	if cfg, err = cfg.Resolve(); err != nil {
		return badInput(err)
	}
	cfg.Build = buildinfo.Read(Version)

	// Ctrl-C cancels the measurement and lets Run remove its temporary files.
	signalCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, err := perf.Run(signalCtx, cfg)
	if err != nil {
		return fmt.Errorf("perf: %w", err)
	}
	return finishPerf(report, *outPath, out)
}

// finishPerf writes the report, prints the summary, and only then turns a
// finding into a failing exit code.
func finishPerf(report perf.Report, outPath string, out io.Writer) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(outPath, append(data, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprint(out, report.FormatText())
	fmt.Fprintf(out, "report written to %s\n", outPath)
	if problems := report.Problems(); len(problems) > 0 {
		return &exitError{code: exitRegression, err: fmt.Errorf("perf found %d problem(s): %s", len(problems), strings.Join(problems, "; "))}
	}
	return nil
}

// splitList splits a comma-separated flag value strictly: an empty item, which
// a stray or trailing comma produces, is an error rather than being skipped.
func splitList(flagName, raw string) ([]string, error) {
	items := strings.Split(raw, ",")
	for i, item := range items {
		items[i] = strings.TrimSpace(item)
		if items[i] == "" {
			return nil, fmt.Errorf("%s %q has an empty item", flagName, raw)
		}
	}
	return items, nil
}

func parseWorkerCounts(raw string) ([]int, error) {
	items, err := splitList("--workers", raw)
	if err != nil {
		return nil, err
	}
	counts := make([]int, len(items))
	for i, item := range items {
		n, err := strconv.Atoi(item)
		if err != nil {
			return nil, fmt.Errorf("--workers: %q is not a whole number", item)
		}
		if n < 1 {
			return nil, fmt.Errorf("--workers: %d must be positive", n)
		}
		counts[i] = n
	}
	return counts, nil
}

func parseLeaseTTLs(raw string) ([]time.Duration, error) {
	items, err := splitList("--lease-ttls", raw)
	if err != nil {
		return nil, err
	}
	ttls := make([]time.Duration, len(items))
	for i, item := range items {
		ttl, err := time.ParseDuration(item)
		if err != nil {
			return nil, fmt.Errorf("--lease-ttls: %q is not a duration such as 1s or 1500ms", item)
		}
		if ttl <= 0 {
			return nil, fmt.Errorf("--lease-ttls: %s must be positive", item)
		}
		ttls[i] = ttl
	}
	return ttls, nil
}

func joinInts(values []int) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ",")
}

func joinDurations(values []time.Duration) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = v.String()
	}
	return strings.Join(parts, ",")
}
