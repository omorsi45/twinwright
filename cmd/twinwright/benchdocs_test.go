package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"twinwright/internal/perf"
)

// A published benchmark figure is one machine's measurement on one day, and a
// figure typed into a document drifts from the run it came from without anyone
// noticing. The rule this file holds is that the throughput table in
// docs/performance.md is a copy of a committed perf report, row for row, so a
// reader can open the JSON and find every number the table prints.
//
// What it does not hold: the recovery, replay and crash matrix tables. They were
// checked against the same reports by a script when they were written and are
// not read here, and ADR 0034 says so.

const (
	performanceDocPath = "../../docs/performance.md"
	benchmarkReportDir = "../../docs/benchmarks"
)

var (
	// reportCitation is how the Results section says which committed report the
	// table after it was copied from.
	reportCitation = regexp.MustCompile("`docs/benchmarks/([A-Za-z0-9._-]+\\.json)`")
	tableRule      = regexp.MustCompile(`^[\s:|-]+$`)
)

// committedBenchmarkReports decodes every docs/benchmarks/*.json as a perf
// report, keyed by file name. Decoding is strict so that a file which is not a
// perf report (a bench report, an empty object, a report with extra fields)
// fails instead of being read as a report with no rows.
func committedBenchmarkReports(t *testing.T) map[string]perf.Report {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(filepath.FromSlash(benchmarkReportDir), "*.json"))
	if err != nil {
		t.Fatalf("glob %s: %v", benchmarkReportDir, err)
	}
	if len(paths) == 0 {
		t.Fatalf("%s holds no *.json report, so the performance document has nothing to quote", benchmarkReportDir)
	}
	reports := map[string]perf.Report{}
	for _, path := range paths {
		name := filepath.Base(path)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		var report perf.Report
		if err = decoder.Decode(&report); err != nil {
			t.Errorf("%s is not a perf report: %v", name, err)
			continue
		}
		if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
			t.Errorf("%s has data after the report", name)
			continue
		}
		switch {
		case report.SchemaVersion < 1 || report.SchemaVersion > perf.SchemaVersion:
			t.Errorf("%s has schema_version %d, want 1 to %d", name, report.SchemaVersion, perf.SchemaVersion)
		case report.Backend != "sqlite" && report.Backend != "postgres":
			t.Errorf("%s has backend %q, want sqlite or postgres", name, report.Backend)
		case report.Environment.Revision == "":
			t.Errorf("%s records no git revision, so nothing says what built the binary", name)
		case len(report.Throughput) == 0:
			t.Errorf("%s has no throughput rows for the table to quote", name)
		default:
			reports[name] = report
		}
	}
	return reports
}

// resultsSection returns the lines of the "## Results" section of the
// performance document.
func resultsSection(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(performanceDocPath))
	if err != nil {
		t.Fatalf("read %s: %v", performanceDocPath, err)
	}
	var section []string
	inside := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "## Results":
			inside = true
		case inside && strings.HasPrefix(line, "## "):
			return section
		case inside:
			section = append(section, line)
		}
	}
	if !inside {
		t.Fatalf("%s has no \"## Results\" section", performanceDocPath)
	}
	return section
}

func tableCells(line string) []string {
	cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

// columnOf returns the index of the header cell named name, ignoring case, or -1.
func columnOf(header []string, name string) int {
	for i, cell := range header {
		if strings.EqualFold(cell, name) {
			return i
		}
	}
	return -1
}

func TestBenchmarkDocsMatchTheirReports(t *testing.T) {
	reports := committedBenchmarkReports(t)
	section := resultsSection(t)

	cited := ""         // the report the most recent citation names
	var header []string // the throughput table's header, once one has started
	columns := map[string]int{}
	quoted := 0

	for number, line := range section {
		where := performanceDocPath + " Results line " + strconv.Itoa(number+1)
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			header = nil
			if m := reportCitation.FindStringSubmatch(line); m != nil {
				cited = m[1]
			}
			continue
		}
		cells := tableCells(line)
		if header == nil {
			// The first line of a table is its header. Only a table with a
			// "Runs/s median" column is a throughput table.
			if columnOf(cells, "Runs/s median") < 0 {
				header = []string{}
				continue
			}
			header = cells
			for _, name := range []string{"Workers", "Runs/s min", "Runs/s median", "Runs/s max"} {
				columns[name] = columnOf(header, name)
				if columns[name] < 0 {
					t.Fatalf("%s: the throughput table has no %q column: %v", where, name, header)
				}
			}
			continue
		}
		if len(header) == 0 || tableRule.MatchString(line) {
			continue // another kind of table, or the rule under a header
		}

		report, ok := reports[cited]
		if !ok {
			t.Errorf("%s: a throughput row follows a citation of %q, which is not a committed report; cite it as `docs/benchmarks/<file>.json` above the table", where, cited)
			continue
		}
		if len(cells) != len(header) {
			t.Errorf("%s: the row has %d cells and the header has %d", where, len(cells), len(header))
			continue
		}
		workers, err := strconv.Atoi(cells[columns["Workers"]])
		if err != nil {
			t.Errorf("%s: workers %q is not an integer", where, cells[columns["Workers"]])
			continue
		}
		var want *perf.ThroughputResult
		for i := range report.Throughput {
			if report.Throughput[i].Workers == workers {
				want = &report.Throughput[i]
			}
		}
		if want == nil {
			t.Errorf("%s: %s has no throughput row for %d worker(s)", where, cited, workers)
			continue
		}
		quoted++
		for _, check := range []struct {
			column string
			want   float64
		}{
			{"Runs/s min", want.RunsPerSecond.Min},
			{"Runs/s median", want.RunsPerSecond.Median},
			{"Runs/s max", want.RunsPerSecond.Max},
		} {
			cell := cells[columns[check.column]]
			got, err := strconv.ParseFloat(cell, 64)
			if err != nil {
				t.Errorf("%s: %s %q is not a number", where, check.column, cell)
				continue
			}
			if got != check.want {
				t.Errorf("%s: %d worker(s), %s is %v in the table and %v in %s", where, workers, check.column, got, check.want, cited)
			}
		}
	}
	if quoted == 0 {
		t.Errorf("%s quotes no runs_per_second median, so this check would pass on nothing; the Results section needs a table headed \"Runs/s median\" under a `docs/benchmarks/<file>.json` citation", performanceDocPath)
	}
}
