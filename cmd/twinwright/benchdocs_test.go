package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"twinwright/internal/perf"
)

// A published benchmark figure is one machine's measurement on one day, and a
// figure typed into a document drifts from the run it came from without anyone
// noticing. The rule this file holds is that the throughput table in
// docs/performance.md is a copy of a committed perf report, row for row, so a
// reader can open the JSON and find every number the table prints, and every row
// of the report is in the table.
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

// throughputProblems compares every throughput table in the Results section
// with the report it cites and returns each difference. It holds three things:
// every quoted cell equals the report's number for that worker count, the table
// says the report's runs per trial, and the set of worker counts a table quotes
// is exactly the report's, so an omitted or repeated row fails as well as an
// altered one. A committed report that no table quotes fails too. quoted is how
// many rows were compared, so a caller can refuse a document that quotes nothing.
func throughputProblems(section []string, reports map[string]perf.Report) (problems []string, quoted int) {
	cited := ""         // the report the most recent citation names
	var header []string // the throughput table's header, once one has started
	columns := map[string]int{}
	rowsOf := map[string]map[int]int{} // report -> worker count -> rows that quote it

	for number, line := range section {
		where := performanceDocPath + " Results line " + strconv.Itoa(number+1)
		problem := func(format string, args ...any) {
			problems = append(problems, where+": "+fmt.Sprintf(format, args...))
		}
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
			for _, name := range []string{"Workers", "Runs per trial", "Runs/s min", "Runs/s median", "Runs/s max"} {
				columns[name] = columnOf(header, name)
				if columns[name] < 0 {
					problem("the throughput table has no %q column: %v", name, header)
					return problems, quoted
				}
			}
			continue
		}
		if len(header) == 0 || tableRule.MatchString(line) {
			continue // another kind of table, or the rule under a header
		}

		report, ok := reports[cited]
		if !ok {
			problem("a throughput row follows a citation of %q, which is not a committed report; cite it as `docs/benchmarks/<file>.json` above the table", cited)
			continue
		}
		if len(cells) != len(header) {
			problem("the row has %d cells and the header has %d", len(cells), len(header))
			continue
		}
		workers, err := strconv.Atoi(cells[columns["Workers"]])
		if err != nil {
			problem("workers %q is not an integer", cells[columns["Workers"]])
			continue
		}
		var want *perf.ThroughputResult
		for i := range report.Throughput {
			if report.Throughput[i].Workers == workers {
				want = &report.Throughput[i]
			}
		}
		if want == nil {
			problem("%s has no throughput row for %d worker(s)", cited, workers)
			continue
		}
		quoted++
		if rowsOf[cited] == nil {
			rowsOf[cited] = map[int]int{}
		}
		rowsOf[cited][workers]++
		if runs, err := strconv.Atoi(cells[columns["Runs per trial"]]); err != nil {
			problem("runs per trial %q is not an integer", cells[columns["Runs per trial"]])
		} else if runs != want.Runs {
			problem("%d worker(s), runs per trial is %d in the table and %d in %s", workers, runs, want.Runs, cited)
		}
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
				problem("%s %q is not a number", check.column, cell)
				continue
			}
			if got != check.want {
				problem("%d worker(s), %s is %v in the table and %v in %s", workers, check.column, got, check.want, cited)
			}
		}
	}

	for _, name := range slices.Sorted(maps.Keys(reports)) {
		rows := rowsOf[name]
		if len(rows) == 0 {
			problems = append(problems, fmt.Sprintf("%s: no throughput table cites %s, so none of its rows is quoted", performanceDocPath, name))
			continue
		}
		for _, row := range reports[name].Throughput {
			switch rows[row.Workers] {
			case 0:
				problems = append(problems, fmt.Sprintf("%s: the table citing %s omits its %d-worker row", performanceDocPath, name, row.Workers))
			case 1:
			default:
				problems = append(problems, fmt.Sprintf("%s: the table citing %s quotes the %d-worker row %d times", performanceDocPath, name, row.Workers, rows[row.Workers]))
			}
		}
	}
	return problems, quoted
}

func TestBenchmarkDocsMatchTheirReports(t *testing.T) {
	problems, quoted := throughputProblems(resultsSection(t), committedBenchmarkReports(t))
	for _, problem := range problems {
		t.Error(problem)
	}
	if quoted == 0 {
		t.Errorf("%s quotes no runs_per_second median, so this check would pass on nothing; the Results section needs a table headed \"Runs/s median\" under a `docs/benchmarks/<file>.json` citation", performanceDocPath)
	}
}

// throughputTables returns, for each throughput table in the section, the index
// of its header line and the index one past its last row.
func throughputTables(section []string) [][2]int {
	var tables [][2]int
	for i := 0; i < len(section); i++ {
		if !strings.HasPrefix(strings.TrimSpace(section[i]), "|") || columnOf(tableCells(section[i]), "Runs/s median") < 0 {
			continue
		}
		end := i
		for end < len(section) && strings.HasPrefix(strings.TrimSpace(section[end]), "|") {
			end++
		}
		tables = append(tables, [2]int{i, end})
		i = end
	}
	return tables
}

// The check above is only worth having if it fails when the table and the report
// part ways in the ways a copy goes wrong without anyone editing a number: a row
// dropped, a column left stale, a whole table removed. Each case here edits the
// real section the way a careless edit would and requires the comparison to
// notice. The first case is the control: the section as committed has no problem.
func TestBenchmarkDocsCheckNoticesWhatItWasWrittenToCatch(t *testing.T) {
	reports := committedBenchmarkReports(t)
	section := resultsSection(t)
	tables := throughputTables(section)
	if len(tables) < 2 {
		t.Fatalf("the Results section has %d throughput tables, want one per committed report", len(tables))
	}
	if problems, _ := throughputProblems(section, reports); len(problems) != 0 {
		t.Fatalf("the committed document already has problems: %v", problems)
	}

	first, last := tables[0], tables[len(tables)-1]
	tests := []struct {
		name   string
		mutate func(lines []string) []string
		want   string
	}{
		{
			name: "the last row of a table is deleted",
			mutate: func(lines []string) []string {
				return slices.Delete(lines, first[1]-1, first[1])
			},
			want: "omits its",
		},
		{
			name: "a row is repeated",
			mutate: func(lines []string) []string {
				return slices.Insert(lines, first[1], lines[first[1]-1])
			},
			want: "times",
		},
		{
			name: "runs per trial is left stale",
			mutate: func(lines []string) []string {
				header := tableCells(lines[first[0]])
				cells := tableCells(lines[first[0]+2])
				cells[columnOf(header, "Runs per trial")] = "400"
				lines[first[0]+2] = "| " + strings.Join(cells, " | ") + " |"
				return lines
			},
			want: "runs per trial",
		},
		{
			name: "a whole table is removed",
			mutate: func(lines []string) []string {
				return slices.Delete(lines, last[0], last[1])
			},
			want: "no throughput table cites",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			edited := tc.mutate(slices.Clone(section))
			problems, _ := throughputProblems(edited, reports)
			if len(problems) == 0 {
				t.Fatal("the comparison passed a document that no longer matches its reports")
			}
			if joined := strings.Join(problems, "\n"); !strings.Contains(joined, tc.want) {
				t.Errorf("no problem mentions %q:\n%s", tc.want, joined)
			}
		})
	}
}
