package gate

import (
	"fmt"
	"strings"
)

// Markdown renders the verdict for a pull request comment or a CI job summary.
// The status word comes first because it is what a reader scans for, and an
// empty section is omitted so a clean gate is a few lines long.
func (r Result) Markdown() string {
	var b strings.Builder
	status := "PASSED"
	if !r.Passed {
		status = "FAILED"
	}
	fmt.Fprintf(&b, "## Twinwright gate: %s\n\n", status)
	fmt.Fprintf(&b, "Suite `%s`: %d cases compared, %d regressions, %d warnings, %d improvements.\n",
		r.Candidate, r.Compared, len(r.Regressions), len(r.Warnings), len(r.Improvements))
	section(&b, "Regressions", r.Regressions)
	section(&b, "Warnings", r.Warnings)
	section(&b, "Improvements", r.Improvements)
	return b.String()
}

func section(b *strings.Builder, title string, findings []Finding) {
	if len(findings) == 0 {
		return
	}
	fmt.Fprintf(b, "\n### %s\n\n| Case | Kind | Detail |\n| --- | --- | --- |\n", title)
	for _, f := range findings {
		name := f.Case
		if name == "" {
			name = "(suite)"
		}
		fmt.Fprintf(b, "| %s | %s | %s |\n", cell(name), cell(f.Kind), cell(f.Detail))
	}
}

// cell keeps a value inside one table cell: a pipe would start a new column and
// a newline would end the row.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.ReplaceAll(s, "\n", " ")
}
