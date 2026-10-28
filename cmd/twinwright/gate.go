package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"twinwright/internal/bench"
	"twinwright/internal/gate"
)

// Exit codes of `twinwright gate`. They are distinct so a CI step can tell "the
// candidate regressed" (block the merge) from "the gate could not decide" (fix
// the pipeline) without parsing text.
const (
	exitRegression = 1
	exitBadInput   = 2
)

// exitError is an error that carries the process exit code main should use.
// Every other failure keeps exit code 1, so existing commands are unchanged.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// exitCode is the process exit code for err: 0 for nil, the carried code for an
// exitError, and 1 otherwise.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var coded *exitError
	if errors.As(err, &coded) {
		return coded.code
	}
	return 1
}

func badInput(err error) error { return &exitError{code: exitBadInput, err: err} }

// gateCommand compares a candidate bench report with a baseline and fails when
// the candidate regressed. The Result JSON is always printed first, so a CI log
// shows what regressed even when the step fails.
func gateCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("gate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	baselinePath := fs.String("baseline", "", "approved bench report JSON")
	candidatePath := fs.String("candidate", "", "bench report JSON to check")
	tolerance := fs.Float64("tolerance", 0, "allowed drop in a case's pass rate, 0 to 1")
	trajectory := fs.String("trajectory", gate.TrajectoryWarn, "a changed tool-call sequence is a fail, warn or ignore")
	markdownPath := fs.String("markdown", "", "also write a Markdown summary to this path, or - for stdout")
	junitPath := fs.String("junit", "", "also write JUnit XML to this path, or - for stdout")
	if err := fs.Parse(args); err != nil {
		return badInput(fmt.Errorf("usage: twinwright gate --baseline A.json --candidate B.json [--tolerance 0] [--trajectory warn] [--markdown PATH] [--junit PATH]: %w", err))
	}
	if fs.NArg() > 0 {
		return badInput(fmt.Errorf("gate takes no positional arguments, got %q", fs.Arg(0)))
	}
	if *baselinePath == "" || *candidatePath == "" {
		return badInput(fmt.Errorf("gate requires --baseline and --candidate"))
	}
	baseline, err := readBenchReport(*baselinePath)
	if err != nil {
		return badInput(err)
	}
	candidate, err := readBenchReport(*candidatePath)
	if err != nil {
		return badInput(err)
	}
	result, err := gate.Compare(baseline, candidate, gate.Options{Tolerance: *tolerance, Trajectory: *trajectory})
	if err != nil {
		return badInput(err)
	}
	if err := writeJSON(out, result); err != nil {
		return badInput(err)
	}
	if *markdownPath != "" {
		if err := writeGateOutput(*markdownPath, []byte(result.Markdown()), out); err != nil {
			return badInput(err)
		}
	}
	if *junitPath != "" {
		data, err := result.JUnit()
		if err != nil {
			return badInput(err)
		}
		if err := writeGateOutput(*junitPath, data, out); err != nil {
			return badInput(err)
		}
	}
	if !result.Passed {
		return &exitError{code: exitRegression, err: fmt.Errorf("gate failed: %d regression(s) against baseline %q", len(result.Regressions), result.Baseline)}
	}
	return nil
}

func readBenchReport(path string) (bench.Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return bench.Report{}, err
	}
	report, err := bench.DecodeReport(data)
	if err != nil {
		return bench.Report{}, fmt.Errorf("%s: %w", path, err)
	}
	return report, nil
}

// writeGateOutput writes to path, or to out when path is "-".
func writeGateOutput(path string, data []byte, out io.Writer) error {
	if path == "-" {
		_, err := out.Write(data)
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
