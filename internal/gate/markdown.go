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
	fmt.Fprintf(&b, "Suite %s: %d cases compared, %d regressions, %d warnings, %d improvements.\n",
		codeSpan(r.Candidate), r.Compared, len(r.Regressions), len(r.Warnings), len(r.Improvements))
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

// codeSpan renders s as one inline code span. A line break would start a new
// line of the summary, so it becomes a space, and the delimiter is a backtick
// run longer than any run inside s, so a backtick in the name cannot end the
// span early. CommonMark strips one space from each end of a span whose content
// has a space at both, which is how a name that starts or ends with a backtick
// is held.
func codeSpan(s string) string {
	s = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(s)
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	fence := strings.Repeat("`", longest+1)
	return fence + s + fence
}
