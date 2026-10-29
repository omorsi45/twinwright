package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"gopkg.in/yaml.v3"

	"twinwright/internal/crashmatrix"
	"twinwright/internal/store"
	"twinwright/internal/worker"
)

// docs/claims.md is the list of everything the README says is true, with the
// test or CI job that holds each line. A list like that rots in a specific way:
// a test is renamed or deleted, a workflow job is dropped, and the row keeps
// citing something that no longer exists, so the document keeps claiming a proof
// nobody can run. These tests hold the part that can be checked by a machine:
// that what a row cites exists, that every row cites something or says plainly
// that nothing proves it, that a row which cites a test that skips without an
// environment variable says so in "Where it runs", and that the documents a
// reader follows from the README do not point at files or headings that are not
// there.
//
// What they do not hold: that a cited test is the right test for its claim, or
// that a cited CI job passed. Both are judgements a reader makes from the row.
//
// Each check is a function that returns its problems, and a separate test feeds
// it the mistakes it exists to catch. A check that nothing ever fails proves
// nothing, and the review of this table found three that could not.

const (
	claimsDocPath = "../../docs/claims.md"
	claimsRepo    = "../.."
	// The design has seven goals and six milestone sections, and the README
	// adds claims of its own, so a table shorter than this is not the table.
	minimumClaimRows = 20
)

var (
	claimToken    = regexp.MustCompile("`([^`\n]+)`")
	claimTestName = regexp.MustCompile(`^Test[A-Za-z0-9_]+$`)
	claimJobName  = regexp.MustCompile(`^([A-Za-z0-9_.-]+\.ya?ml)/([A-Za-z0-9_-]+)$`)
	claimEnvVar   = regexp.MustCompile(`TWINWRIGHT_TEST_[A-Z][A-Z_]*`)
	claimTestDecl = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	claimMDLink   = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	// Whole words only: "cut" is also inside "execute".
	claimDeclared  = regexp.MustCompile(`\b(not verified|cut)\b`)
	claimHeader    = []string{"Claim", "Proven by", "Where it runs"}
	claimWhereKind = []string{"local suite", "ci fast lane", "integration", "opt-in", "local run", "none"}

	// claimSkippedDirs hold copies of the repository and scratch files that are
	// not part of what a reader can run: version control, the private working
	// directory, the git worktrees checked out beside the code, and the
	// downloaded tools. A test function that exists only in one of them must not
	// satisfy a citation, and a link target found only in one must not resolve.
	claimSkippedDirs = map[string]bool{".git": true, ".superpowers": true, ".worktrees": true, ".tools": true, "testdata": true}

	claimHeading     = regexp.MustCompile(`^ {0,3}#{1,6}[ \t]+(.*?)(?:[ \t]+#+)?[ \t]*$`)
	claimHeadingLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
)

// collectTests returns every Go test function name declared under root, skipping
// the directories in claimSkippedDirs.
func collectTests(root string) (map[string]bool, error) {
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if claimSkippedDirs[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range claimTestDecl.FindAllStringSubmatch(string(raw), -1) {
			found[m[1]] = true
		}
		return nil
	})
	return found, err
}

func repositoryTests(t *testing.T) map[string]bool {
	t.Helper()
	found, err := collectTests(filepath.FromSlash(claimsRepo))
	if err != nil {
		t.Fatalf("walk %s for test functions: %v", claimsRepo, err)
	}
	if len(found) == 0 {
		t.Fatal("found no test functions, so a cited test could not be checked")
	}
	return found
}

// workflowJobs returns the job IDs of one file in .github/workflows, decoded the
// way GitHub reads it: the keys of the top-level jobs mapping.
func workflowJobs(name string) (map[string]bool, error) {
	raw, err := os.ReadFile(filepath.Join(filepath.FromSlash(claimsRepo), ".github", "workflows", name))
	if err != nil {
		return nil, err
	}
	var workflow struct {
		Jobs map[string]any `yaml:"jobs"`
	}
	if err = yaml.Unmarshal(raw, &workflow); err != nil {
		return nil, err
	}
	jobs := map[string]bool{}
	for id := range workflow.Jobs {
		jobs[id] = true
	}
	return jobs, nil
}

func claimsDocument(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(claimsDocPath))
	if err != nil {
		t.Fatalf("read %s: %v (the claims table is part of the documentation set)", claimsDocPath, err)
	}
	if len(raw) == 0 {
		t.Fatalf("%s is empty", claimsDocPath)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// parseClaimRows returns the data rows of the one table in the document, each as
// its three cells. A second header, a row with another number of cells, or no
// header at all is a problem, because a table that cannot be parsed cannot be
// checked.
func parseClaimRows(text string) (rows [][]string, problems []string) {
	headers := 0
	for number, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		cells := tableCells(line)
		if tableRule.MatchString(strings.TrimSpace(line)) {
			continue
		}
		if len(cells) == len(claimHeader) && cells[0] == claimHeader[0] {
			headers++
			for i, want := range claimHeader {
				if cells[i] != want {
					problems = append(problems, fmt.Sprintf("%s line %d: column %d is %q, want %q", claimsDocPath, number+1, i+1, cells[i], want))
				}
			}
			continue
		}
		if len(cells) != len(claimHeader) {
			problems = append(problems, fmt.Sprintf("%s line %d: a row has %d cells, want %d: %q", claimsDocPath, number+1, len(cells), len(claimHeader), line))
			continue
		}
		rows = append(rows, cells)
	}
	if headers != 1 {
		problems = append(problems, fmt.Sprintf("%s has %d header rows, want exactly 1 (%s)", claimsDocPath, headers, strings.Join(claimHeader, " | ")))
	}
	return rows, problems
}

func backticked(text string) []string {
	var out []string
	for _, m := range claimToken.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// claimSources is what the claims checks look things up in: the test functions
// that exist, the jobs of each workflow file, and the environment variables a
// test needs before it runs.
type claimSources struct {
	tests map[string]bool
	jobs  func(file string) (map[string]bool, error)
	gates func(test string) []string
}

// claimProblems returns every way the claims document fails its rules.
func claimProblems(text string, src claimSources) []string {
	var problems []string

	// Everything backticked anywhere in the document is held, not only the cells
	// of the table, so a test named in a sentence under the table is checked too.
	checkedTests, checkedJobs := 0, 0
	jobsByFile := map[string]map[string]bool{}
	for _, token := range backticked(text) {
		switch {
		case claimTestName.MatchString(token):
			checkedTests++
			if !src.tests[token] {
				problems = append(problems, fmt.Sprintf("%s cites %s, which is not declared as func %s( in any _test.go file", claimsDocPath, token, token))
			}
		case claimJobName.MatchString(token):
			checkedJobs++
			parts := claimJobName.FindStringSubmatch(token)
			file, job := parts[1], parts[2]
			jobs, seen := jobsByFile[file]
			if !seen {
				var err error
				jobs, err = src.jobs(file)
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s cites %s, but .github/workflows/%s cannot be read as a workflow: %v", claimsDocPath, token, file, err))
				}
				jobsByFile[file] = jobs
			}
			if jobs != nil && !jobs[job] {
				known := make([]string, 0, len(jobs))
				for id := range jobs {
					known = append(known, id)
				}
				sort.Strings(known)
				problems = append(problems, fmt.Sprintf("%s cites %s, but %s has no job %q (jobs: %s)", claimsDocPath, token, file, job, strings.Join(known, ", ")))
			}
		}
	}
	if checkedTests == 0 || checkedJobs == 0 {
		problems = append(problems, fmt.Sprintf("%s cites %d tests and %d CI jobs: a claims table that names neither kind of proof proves nothing", claimsDocPath, checkedTests, checkedJobs))
	}

	rows, rowProblems := parseClaimRows(text)
	problems = append(problems, rowProblems...)
	if len(rows) < minimumClaimRows {
		problems = append(problems, fmt.Sprintf("%s has %d claim rows, want at least %d", claimsDocPath, len(rows), minimumClaimRows))
	}
	for _, row := range rows {
		claim, proof, where := row[0], row[1], row[2]
		if claim == "" || proof == "" || where == "" {
			problems = append(problems, fmt.Sprintf("a row has an empty cell: %q", row))
			continue
		}
		cited := false
		var viaCI, viaIntegration bool
		var citedTests []string
		for _, token := range backticked(proof) {
			if claimTestName.MatchString(token) {
				cited = true
				citedTests = append(citedTests, token)
			}
			if parts := claimJobName.FindStringSubmatch(token); parts != nil {
				cited = true
				viaCI = viaCI || parts[1] == "ci.yml"
				viaIntegration = viaIntegration || parts[1] == "integration.yml"
			}
		}
		// "not verified" and "cut" are claims in their own right: the row says
		// plainly that nothing proves this or that the scope was dropped. What
		// is not allowed is a row that says nothing either way.
		declared := claimDeclared.MatchString(proof)
		if !cited && !declared {
			problems = append(problems, fmt.Sprintf("row %q cites no test or CI job and does not say \"not verified\" or \"cut\"", claim))
		}

		lowerWhere := strings.ToLower(where)
		kind := false
		for _, name := range claimWhereKind {
			kind = kind || strings.Contains(lowerWhere, name)
		}
		if !kind {
			problems = append(problems, fmt.Sprintf("row %q: %q is none of %s", claim, where, strings.Join(claimWhereKind, ", ")))
		}
		if strings.Contains(lowerWhere, "opt-in") && !claimEnvVar.MatchString(where) {
			problems = append(problems, fmt.Sprintf("row %q runs opt-in but names no TWINWRIGHT_TEST_ environment variable: %q", claim, where))
		}
		if strings.Contains(lowerWhere, "local run") && !strings.Contains(where, ".md") {
			problems = append(problems, fmt.Sprintf("row %q is a local run but names no document: %q", claim, where))
		}
		if viaCI && !strings.Contains(lowerWhere, "ci fast lane") {
			problems = append(problems, fmt.Sprintf("row %q cites a ci.yml job but does not say it runs in the CI fast lane: %q", claim, where))
		}
		if viaIntegration && !strings.Contains(lowerWhere, "integration") {
			problems = append(problems, fmt.Sprintf("row %q cites an integration.yml job but does not say it runs in Integration: %q", claim, where))
		}

		// A test that skips without an environment variable does not run in the
		// local suite. The row must name the variable, or say the test runs in
		// Integration, where the workflow sets it.
		if src.gates != nil && !strings.Contains(lowerWhere, "integration") {
			for _, test := range citedTests {
				for _, variable := range src.gates(test) {
					if !strings.Contains(where, variable) {
						problems = append(problems, fmt.Sprintf("row %q cites %s, which skips unless %s is set, but %q names neither that variable nor Integration", claim, test, variable, where))
					}
				}
			}
		}
	}
	return problems
}

func TestClaimsTableReferencesExist(t *testing.T) {
	index, err := buildGoIndex(filepath.FromSlash(claimsRepo))
	if err != nil {
		t.Fatalf("index the Go source: %v", err)
	}
	src := claimSources{tests: repositoryTests(t), jobs: workflowJobs, gates: index.gatesOf}
	for _, problem := range claimProblems(claimsDocument(t), src) {
		t.Error(problem)
	}
}

// The review of this table mutated it and found rows it could not fail. Each
// mutation here is one of those: a citation that no longer exists, a job that
// does not exist, a row that proves nothing, a CI job claimed without the fast
// lane, and an opt-in test claimed as part of the local suite. The unmutated
// table must pass the same check, or the failures would prove nothing.
func TestClaimsChecksCatchTheReviewedMutations(t *testing.T) {
	index, err := buildGoIndex(filepath.FromSlash(claimsRepo))
	if err != nil {
		t.Fatalf("index the Go source: %v", err)
	}
	src := claimSources{tests: repositoryTests(t), jobs: workflowJobs, gates: index.gatesOf}
	text := claimsDocument(t)
	if problems := claimProblems(text, src); len(problems) != 0 {
		t.Fatalf("the unmutated table has problems, so a mutation could not be told apart: %q", problems)
	}

	mutations := []struct {
		name     string
		old, new string
		want     string
	}{
		{"a cited test is renamed to one that does not exist",
			"`TestRunFailOnEval`", "`TestRunFailOnEvalRenamed`", "TestRunFailOnEvalRenamed"},
		{"a cited job does not exist",
			"`integration.yml/perf`", "`integration.yml/perf-gone`", "perf-gone"},
		{"a row cites nothing and declares nothing",
			"| `TestTrialsRefusedOutsideLocalMode` | local suite |", "| it just works | local suite |", "cites no test or CI job"},
		{"a ci.yml job is cited without the CI fast lane",
			"`ci.yml/regression-gate` | local suite; CI fast lane |", "`ci.yml/regression-gate` | local suite |", "CI fast lane"},
		{"tests that skip without TWINWRIGHT_TEST_OTEL are claimed as local suite",
			"| `TestSpansAreQueryableInJaeger`; `TestPrometheusScrapesAWorker`; `integration.yml/observability` | opt-in (`TWINWRIGHT_TEST_OTEL=1`); Integration |",
			"| `TestSpansAreQueryableInJaeger`; `TestPrometheusScrapesAWorker` | local suite |", "TWINWRIGHT_TEST_OTEL"},
		{"a PostgreSQL test is claimed as local suite",
			"| `TestCrashMatrixPostgres`; `TestRealProcessDeathIsRecoveredPostgres`; `integration.yml/postgres` | opt-in (`TWINWRIGHT_TEST_POSTGRES_DSN`); Integration |",
			"| `TestCrashMatrixPostgres`; `TestRealProcessDeathIsRecoveredPostgres` | local suite |", "TWINWRIGHT_TEST_POSTGRES_DSN"},
		{"a Docker test is claimed as local suite",
			"| `TestDockerStartsTheExampleSidecarAndItIsActuallyListening`; `TestDockerFailedStartLeavesNothingBehind`; `integration.yml/docker` | opt-in (`TWINWRIGHT_TEST_DOCKER=1`); Integration |",
			"| `TestDockerStartsTheExampleSidecarAndItIsActuallyListening`; `TestDockerFailedStartLeavesNothingBehind` | local suite |", "TWINWRIGHT_TEST_DOCKER"},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			if strings.Count(text, m.old) < 1 {
				t.Fatalf("the table no longer holds the text this mutation replaces, so it tests nothing: %q", m.old)
			}
			problems := claimProblems(strings.Replace(text, m.old, m.new, 1), src)
			for _, problem := range problems {
				if strings.Contains(problem, m.want) {
					return
				}
			}
			t.Errorf("the check did not report %q after the mutation; it reported %q", m.want, problems)
		})
	}
}

// A relative link that points at a file that is not there is the commonest way a
// restructured document set breaks, and the README's rewrite moves a thousand
// lines of content into other files. A link to a heading that does not exist
// lands on the top of the page and looks fine until someone follows it. Every
// link a reader can follow from the README, the architecture page, the claims
// table and the guide must land on its file and on its heading.
func TestDocLinksResolve(t *testing.T) {
	root := filepath.FromSlash(claimsRepo)
	required := []string{"README.md", filepath.Join("docs", "architecture.md"), filepath.Join("docs", "claims.md")}
	var files []string
	for _, name := range required {
		path := filepath.Join(root, name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s is part of the documentation set and is missing: %v", name, err)
			continue
		}
		files = append(files, path)
	}
	guide, err := filepath.Glob(filepath.Join(root, "docs", "guide", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(guide) == 0 {
		t.Error("docs/guide holds no Markdown file: the README's detail has to live somewhere a link can reach")
	}
	files = append(files, guide...)

	checked := 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		count, problems := linkProblems(path, string(raw))
		checked += count
		for _, problem := range problems {
			t.Error(problem)
		}
	}
	if len(files) > 0 && checked == 0 {
		t.Error("no relative link was checked, so this test would pass on documents that link to nothing")
	}
}

// linkProblems checks every relative link of one Markdown document, given as
// its path and its text: the target file must exist and, when the link names a
// heading, the target must have it. It returns how many links it checked.
func linkProblems(path, text string) (checked int, problems []string) {
	inFence := false
	anchors := map[string]map[string]bool{}
	anchorsOf := func(file string) (map[string]bool, error) {
		if found, ok := anchors[file]; ok {
			return found, nil
		}
		raw, err := os.ReadFile(file)
		if file == path {
			raw, err = []byte(text), nil
		}
		if err != nil {
			return nil, err
		}
		anchors[file] = headingAnchors(string(raw))
		return anchors[file], nil
	}
	for number, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		for _, m := range claimMDLink.FindAllStringSubmatch(line, -1) {
			target, fragment, hasFragment := strings.Cut(m[1], "#")
			if strings.HasPrefix(target, "http://") ||
				strings.HasPrefix(target, "https://") ||
				strings.HasPrefix(target, "mailto:") {
				continue
			}
			if target == "" && !hasFragment {
				continue
			}
			checked++
			resolved := path
			if target != "" {
				resolved = filepath.Join(filepath.Dir(path), filepath.FromSlash(target))
				if _, err := os.Stat(resolved); err != nil {
					problems = append(problems, fmt.Sprintf("%s line %d links to %s, which does not exist", filepath.ToSlash(path), number+1, m[1]))
					continue
				}
			}
			// Only a Markdown target has headings to check.
			if fragment == "" || !strings.HasSuffix(resolved, ".md") {
				continue
			}
			found, err := anchorsOf(resolved)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s line %d links to %s, which cannot be read: %v", filepath.ToSlash(path), number+1, m[1], err))
				continue
			}
			if !found[fragment] {
				problems = append(problems, fmt.Sprintf("%s line %d links to %s, but %s has no heading that GitHub gives the anchor #%s", filepath.ToSlash(path), number+1, m[1], filepath.ToSlash(resolved), fragment))
			}
		}
	}
	return checked, problems
}

// headingAnchors returns the anchors GitHub gives the headings of a Markdown
// document: the heading text lowercased, spaces turned into hyphens and
// punctuation other than hyphens and underscores dropped, with -1, -2 and so on
// added to a heading whose anchor an earlier one already took. Lines inside a
// code fence are not headings.
func headingAnchors(text string) map[string]bool {
	anchors := map[string]bool{}
	seen := map[string]int{}
	inFence := false
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		m := claimHeading.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		slug := headingSlug(m[1])
		anchor := slug
		if n := seen[slug]; n > 0 {
			anchor = fmt.Sprintf("%s-%d", slug, n)
		}
		seen[slug]++
		anchors[anchor] = true
	}
	return anchors
}

func headingSlug(heading string) string {
	heading = claimHeadingLink.ReplaceAllString(heading, "$1")
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(heading)) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.Is(unicode.M, r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestHeadingAnchorsFollowGitHubsRules(t *testing.T) {
	text := strings.Join([]string{
		"# Title",
		"## Limits",
		"## `--fail-on-eval` and `--enqueue`",
		"## What it does (and does not)",
		"### Snake_case and CamelCase",
		"## [Linked](other.md) heading",
		"## Limits",
		"## Limits",
		"## Closed heading ##",
		"```",
		"## Inside a fence",
		"```",
		"    ## indented four spaces is code",
		"Not a heading # here",
		"## Naïve résumé: 3 steps, 4 weeks",
	}, "\n")
	got := headingAnchors(text)
	want := []string{
		"title", "limits", "limits-1", "limits-2", "--fail-on-eval-and---enqueue",
		"what-it-does-and-does-not", "snake_case-and-camelcase", "linked-heading",
		"closed-heading", "naïve-résumé-3-steps-4-weeks",
	}
	for _, anchor := range want {
		if !got[anchor] {
			t.Errorf("missing anchor %q in %v", anchor, got)
		}
	}
	for _, anchor := range []string{"inside-a-fence", "indented-four-spaces-is-code", "limits-3", "here"} {
		if got[anchor] {
			t.Errorf("anchor %q should not exist, got %v", anchor, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d anchors %v, want %d", len(got), got, len(want))
	}
}

// A link whose #anchor names no heading is the second gap the review found: the
// file check passed it, and a reader lands at the top of a long page.
func TestDocLinkAnchorsAreChecked(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("target.md", "# Target\n\n## The limits\n\n```\n## Not a heading\n```\n")
	source := write("source.md", "# Source\n\n## Local section\n")
	const head = "# Source\n\n## Local section\n\n"
	good := head + "[a](target.md#the-limits) [b](target.md) [c](#local-section) [d](https://example.com/x#nowhere) [e](target.md#target)\n"
	checked, problems := linkProblems(source, good)
	if len(problems) != 0 || checked != 4 {
		t.Errorf("good links: checked %d, problems %q, want 4 checked and none", checked, problems)
	}
	for name, link := range map[string]string{
		"a heading that does not exist": "[x](target.md#no-such-heading)",
		"a heading inside a fence":      "[x](target.md#not-a-heading)",
		"the wrong case":                "[x](target.md#The-limits)",
		"a local heading that is gone":  "[x](#gone)",
		"a missing file":                "[x](missing.md#target)",
	} {
		t.Run(name, func(t *testing.T) {
			if _, problems := linkProblems(source, head+link+"\n"); len(problems) != 1 {
				t.Errorf("problems = %q, want exactly one for %s", problems, link)
			}
		})
	}

	// The reviewer's mutation: an anchor in the real claims table that names no
	// heading. The unmutated page must pass the same check.
	raw := claimsDocument(t)
	path := filepath.FromSlash(claimsDocPath)
	if _, problems := linkProblems(path, raw); len(problems) != 0 {
		t.Fatalf("the unmutated claims page has link problems: %q", problems)
	}
	const anchor = "adr/0032-observability-verified.md#limits"
	if strings.Count(raw, anchor) == 0 {
		t.Fatalf("the claims page no longer links to %s, so this mutation tests nothing", anchor)
	}
	mutated := strings.Replace(raw, anchor, "adr/0032-observability-verified.md#no-such-heading", 1)
	if _, problems := linkProblems(path, mutated); len(problems) != 1 || !strings.Contains(problems[0], "no-such-heading") {
		t.Errorf("a claims link to a heading that does not exist was not reported once: %q", problems)
	}
}

// A test function that exists only in a git worktree or a downloaded tool must
// not satisfy a citation: CI would not have it.
func TestCollectTestsIgnoresWorktreesAndTools(t *testing.T) {
	root := t.TempDir()
	write := func(rel, text string) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/a/a_test.go", "package a\n\nfunc TestReal(t *testing.T) {}\n")
	write(".worktrees/branch/internal/a/a_test.go", "package a\n\nfunc TestOnlyInAWorktree(t *testing.T) {}\n")
	write(".tools/x/y_test.go", "package y\n\nfunc TestOnlyInATool(t *testing.T) {}\n")
	write(".superpowers/z_test.go", "package z\n\nfunc TestOnlyInScratch(t *testing.T) {}\n")
	write(".git/q_test.go", "package q\n\nfunc TestOnlyInGit(t *testing.T) {}\n")
	found, err := collectTests(root)
	if err != nil {
		t.Fatal(err)
	}
	if !found["TestReal"] || len(found) != 1 {
		t.Errorf("collected %v, want only TestReal", found)
	}
}

// goIndex is a name-level index of the Go source under a root: enough to tell
// which TWINWRIGHT_TEST_ variable a test needs before it runs, without running
// it. It parses; it does not type-check, so a function is found by its name in
// its own directory or through an import of a package in the same module.
type goIndex struct {
	module string
	dirs   map[string]*goDir
}

type goDir struct {
	// consts holds the package-level string constants whose value names a
	// TWINWRIGHT_TEST_ variable, by constant name.
	consts map[string]string
	funcs  map[string][]*goFunc
}

type goFunc struct {
	decl    *ast.FuncDecl
	dir     string
	imports map[string]string // import name in the declaring file -> directory
	isTest  bool
}

func buildGoIndex(root string) (*goIndex, error) {
	module := ""
	if raw, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
				module = strings.TrimSpace(rest)
				break
			}
		}
	}
	index := &goIndex{module: module, dirs: map[string]*goDir{}}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if claimSkippedDirs[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		dirKey := filepath.ToSlash(rel)
		dir := index.dirs[dirKey]
		if dir == nil {
			dir = &goDir{consts: map[string]string{}, funcs: map[string][]*goFunc{}}
			index.dirs[dirKey] = dir
		}
		imports := map[string]string{}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil || module == "" || !strings.HasPrefix(importPath, module+"/") {
				continue
			}
			name := importPath[strings.LastIndex(importPath, "/")+1:]
			if spec.Name != nil {
				name = spec.Name.Name
			}
			imports[name] = strings.TrimPrefix(importPath, module+"/")
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != token.CONST {
					continue
				}
				for _, spec := range d.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range value.Names {
						if i >= len(value.Values) {
							continue
						}
						if lit, ok := value.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							text, err := strconv.Unquote(lit.Value)
							if err == nil && claimEnvVar.MatchString(text) {
								dir.consts[name.Name] = claimEnvVar.FindString(text)
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Body == nil {
					continue
				}
				dir.funcs[d.Name.Name] = append(dir.funcs[d.Name.Name], &goFunc{
					decl: d, dir: dirKey, imports: imports,
					isTest: strings.HasSuffix(path, "_test.go"),
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return index, nil
}

// mentions returns the TWINWRIGHT_TEST_ variables a function's body names: as a
// string literal, or through a constant of its own package or of a package it
// imports from this module.
func (ix *goIndex) mentions(f *goFunc) map[string]bool {
	found := map[string]bool{}
	ast.Inspect(f.decl.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				text, err := strconv.Unquote(x.Value)
				if err != nil {
					text = x.Value
				}
				for _, v := range claimEnvVar.FindAllString(text, -1) {
					found[v] = true
				}
			}
		case *ast.Ident:
			if v, ok := ix.dirs[f.dir].consts[x.Name]; ok {
				found[v] = true
			}
		case *ast.SelectorExpr:
			if pkg, ok := x.X.(*ast.Ident); ok {
				if dir := ix.dirs[f.imports[pkg.Name]]; dir != nil {
					if v, ok := dir.consts[x.Sel.Name]; ok {
						found[v] = true
					}
				}
			}
		}
		return true
	})
	return found
}

func skips(f *goFunc) bool {
	skipping := false
	ast.Inspect(f.decl.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "Skip", "Skipf", "SkipNow":
					skipping = true
				}
			}
		}
		return !skipping
	})
	return skipping
}

// callees returns the functions a body calls by name, in its own directory or
// through an import from this module. With unconditional set it leaves out calls
// that sit inside a branch, a loop, a switch or a function literal, because
// those do not happen on every run of the caller: a helper that opens a
// PostgreSQL schema only when pgtest.Available() says one is configured must
// not make every test that uses it opt-in.
func (ix *goIndex) callees(f *goFunc, unconditional bool) []*goFunc {
	var out []*goFunc
	ast.Inspect(f.decl.Body, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.FuncLit, *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
			if unconditional {
				return false
			}
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var dir *goDir
		var name string
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			dir, name = ix.dirs[f.dir], fun.Name
		case *ast.SelectorExpr:
			if pkg, ok := fun.X.(*ast.Ident); ok {
				dir, name = ix.dirs[f.imports[pkg.Name]], fun.Sel.Name
			}
		}
		if dir == nil {
			return true
		}
		for _, candidate := range dir.funcs[name] {
			if candidate.decl.Recv == nil {
				out = append(out, candidate)
			}
		}
		return true
	})
	return out
}

type gateResult struct {
	skips bool
	vars  map[string]bool
}

// analyze says whether running f can end in a skip and which TWINWRIGHT_TEST_
// variables that skip depends on. A function skips if it calls Skip itself or
// calls, unconditionally, a function that does. The variables are the ones it
// names (and the ones the functions it calls name in their own bodies) when it
// skips itself, those of an unconditional callee that skips on them, and its own
// when an unconditional callee skips on a value it was handed, as a test does
// when it passes the switch's name to a helper that calls t.Skip.
func (ix *goIndex) analyze(f *goFunc, memo map[*goFunc]*gateResult, active map[*goFunc]bool) *gateResult {
	if r, ok := memo[f]; ok {
		return r
	}
	result := &gateResult{vars: map[string]bool{}}
	if active[f] {
		return result
	}
	active[f] = true
	defer delete(active, f)

	own := ix.mentions(f)
	if skips(f) {
		result.skips = true
		for v := range own {
			result.vars[v] = true
		}
		for _, c := range ix.callees(f, false) {
			for v := range ix.mentions(c) {
				result.vars[v] = true
			}
		}
	}
	calleeSkips := false
	for _, c := range ix.callees(f, true) {
		r := ix.analyze(c, memo, active)
		for v := range r.vars {
			result.vars[v] = true
		}
		calleeSkips = calleeSkips || r.skips
	}
	if calleeSkips {
		result.skips = true
		for v := range own {
			result.vars[v] = true
		}
	}
	memo[f] = result
	return result
}

// gatesOf returns the TWINWRIGHT_TEST_ variables without which the named test
// skips, sorted. It is a name-level reading of the source: a test that reaches
// its skip only through a method call, a callback or a branch is not seen as
// gated, so the check is a floor, not a proof that no other test is opt-in.
func (ix *goIndex) gatesOf(test string) []string {
	memo := map[*goFunc]*gateResult{}
	found := map[string]bool{}
	for _, dir := range ix.dirs {
		for _, f := range dir.funcs[test] {
			if !f.isTest {
				continue
			}
			for v := range ix.analyze(f, memo, map[*goFunc]bool{}).vars {
				found[v] = true
			}
		}
	}
	out := make([]string, 0, len(found))
	for v := range found {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// The gate reader is what makes "Where it runs" checkable, so it is held on the
// tests the table cites today: the ones that need a variable, the ones that look
// like they might and do not, and the ones that skip for another reason.
func TestGateReaderFindsTheVariableATestNeeds(t *testing.T) {
	index, err := buildGoIndex(filepath.FromSlash(claimsRepo))
	if err != nil {
		t.Fatal(err)
	}
	for test, want := range map[string]string{
		// os.Getenv in the test's own body.
		"TestSpansAreQueryableInJaeger": "TWINWRIGHT_TEST_OTEL",
		"TestPrometheusScrapesAWorker":  "TWINWRIGHT_TEST_OTEL",
		// A helper in another package that skips, called with no condition.
		"TestPostgresRuntimeParity": "TWINWRIGHT_TEST_POSTGRES_DSN",
		// A condition on a function that reads the variable.
		"TestCrashMatrixPostgres": "TWINWRIGHT_TEST_POSTGRES_DSN",
		// A constant of the test's own package, read by a helper.
		"TestPythonAgentInHardenedContainer": "TWINWRIGHT_TEST_DOCKER",
		// The switch's name passed to a helper that calls t.Skip.
		"TestLiveOpenAIToolCall":    "TWINWRIGHT_TEST_LIVE_OPENAI",
		"TestLiveAnthropicToolCall": "TWINWRIGHT_TEST_LIVE_ANTHROPIC",
	} {
		if got := index.gatesOf(test); len(got) != 1 || got[0] != want {
			t.Errorf("gatesOf(%s) = %q, want [%s]", test, got, want)
		}
	}
	for _, test := range []string{
		"TestDemo",
		// Runs on PostgreSQL too when one is configured, but is a local test.
		"TestWorkerClaimsAndCompletesAQueuedRun",
		// Skips without python3, which is not a TWINWRIGHT_TEST_ variable.
		"TestPythonReferenceAgent",
		// Names the variables in its table and never skips on them.
		"TestLiveSettingsSkipAndNameWhatIsMissing",
		"TestClaimsTableReferencesExist",
	} {
		if got := index.gatesOf(test); len(got) != 0 {
			t.Errorf("gatesOf(%s) = %q, want none", test, got)
		}
	}
}

// The same reader on a small module, so each shape it must tell apart is shown
// in isolation and a change in the repository's own helpers cannot hide a break.
func TestGateReaderOnASyntheticModule(t *testing.T) {
	root := t.TempDir()
	write := func(rel, text string) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.test/m\n\ngo 1.22\n")
	write("internal/gate/gate.go", `package gate

import (
	"os"
	"testing"
)

const Env = "TWINWRIGHT_TEST_ALPHA"

func Require(t testing.TB) {
	if os.Getenv(Env) != "1" {
		t.Skip("set it")
	}
}

func Available() bool { return os.Getenv(Env) != "" }
`)
	write("pkg/a_test.go", `package pkg

import (
	"os"
	"testing"

	"example.test/m/internal/gate"
)

const beta = "TWINWRIGHT_TEST_BETA"

func helperSkip(t *testing.T, name string) {
	if os.Getenv(name) != "1" {
		t.Skip("off")
	}
}

func checkViaImport(t *testing.T)     { gate.Require(t) }
func checkViaLiteral(t *testing.T)    { if os.Getenv("TWINWRIGHT_TEST_GAMMA") == "" { t.Skip() } }
func checkViaConstArgument(t *testing.T) { helperSkip(t, beta) }
func checkViaCondition(t *testing.T) {
	if !gate.Available() {
		t.Skip("no")
	}
}
func checkBranchOnly(t *testing.T) {
	if gate.Available() {
		gate.Require(t)
	}
}
func checkSubtestOnly(t *testing.T) {
	t.Run("x", func(t *testing.T) { gate.Require(t) })
}
func checkSkipsForAnotherReason(t *testing.T) { t.Skip("python3 is missing") }
func checkMentionsWithoutSkipping(t *testing.T) { _ = "TWINWRIGHT_TEST_DELTA" }
func checkPlain(t *testing.T)           {}
func checkMutualA(t *testing.T)         { checkMutualB(t) }
func checkMutualB(t *testing.T)         { checkMutualA(t) }
`)
	index, err := buildGoIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	for test, want := range map[string][]string{
		"checkViaImport":               {"TWINWRIGHT_TEST_ALPHA"},
		"checkViaLiteral":              {"TWINWRIGHT_TEST_GAMMA"},
		"checkViaConstArgument":        {"TWINWRIGHT_TEST_BETA"},
		"checkViaCondition":            {"TWINWRIGHT_TEST_ALPHA"},
		"checkBranchOnly":              nil,
		"checkSubtestOnly":             nil,
		"checkSkipsForAnotherReason":   nil,
		"checkMentionsWithoutSkipping": nil,
		"checkPlain":                   nil,
		"checkMutualA":                 nil,
		"checkNotDeclaredAnywhere":     nil,
	} {
		got := index.gatesOf(test)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("gatesOf(%s) = %q, want %q", test, got, want)
		}
	}
}

// The README says it links the documentation a reader needs to judge it: the
// claims table, the architecture page, the security review, the performance page
// and the guide. TestDocLinksResolve proves a link that exists lands; it cannot
// notice that a link was deleted. This does.
var readmeRequiredLinks = []string{
	"docs/claims.md",
	"docs/architecture.md",
	"docs/security-review.md",
	"docs/performance.md",
	"docs/guide/README.md",
}

func readmeLinkProblems(text string) []string {
	var problems []string
	for _, target := range readmeRequiredLinks {
		if !strings.Contains(text, "]("+target) {
			problems = append(problems, fmt.Sprintf("README.md has no link to %s", target))
		}
	}
	return problems
}

func TestReadmeLinksTheDocumentationSet(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(filepath.FromSlash(claimsRepo), "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, problem := range readmeLinkProblems(text) {
		t.Error(problem)
	}
	// Each required link, removed in turn, must be noticed.
	for _, target := range readmeRequiredLinks {
		mutated := strings.ReplaceAll(text, "]("+target, "](nowhere")
		problems := readmeLinkProblems(mutated)
		if len(problems) != 1 || !strings.Contains(problems[0], target) {
			t.Errorf("removing the link to %s was reported as %q, want exactly that link", target, problems)
		}
	}
}

// The delivery bound's default is quoted in the README, the fleet page and the
// guide. A constant changed in worker.go would leave all three wrong, and no test
// read them.
func TestDocumentedDeliveryBoundMatchesTheCode(t *testing.T) {
	want := fmt.Sprintf("`MaxAttempts` (default %d)", worker.DefaultMaxAttempts)
	for _, name := range []string{"README.md", "docs/distributed.md", "docs/guide/distributed-runtime.md"} {
		raw, err := os.ReadFile(filepath.Join(filepath.FromSlash(claimsRepo), filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(raw)), " ")
		if !strings.Contains(text, want) {
			t.Errorf("%s does not say %s, the default worker.DefaultMaxAttempts sets", name, want)
		}
	}
}

// The crash matrix is quoted as 120 cases (25, 15 and 80) in the README, the fleet
// page and the guide. TestCrashMatrixSQLite runs whatever the harness enumerates,
// so a harness that enumerated fewer cases would still pass it. This counts them
// and holds each document that quotes the total to the count.
func TestCrashMatrixCasesAreTheOnesTheDocumentsCount(t *testing.T) {
	ctx := context.Background()
	scenarios, err := crashmatrix.Scenarios(filepath.Join(filepath.FromSlash(claimsRepo), "examples"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"duplicate-charge": 25, "ambiguous-commit": 15, "company-incident": 80}
	got := map[string]int{}
	total := 0
	for _, sc := range scenarios {
		s, err := store.Open(filepath.Join(t.TempDir(), sc.Name+".db"))
		if err != nil {
			t.Fatal(err)
		}
		_, cases, err := crashmatrix.Enumerate(ctx, s, sc)
		_ = s.Close()
		if err != nil {
			t.Fatalf("enumerate %s: %v", sc.Name, err)
		}
		got[sc.Name] = len(cases)
		total += len(cases)
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s enumerates %d crash cases, the documents say %d", name, got[name], n)
		}
	}
	if len(got) != len(want) {
		t.Errorf("the matrix covers %d scenarios (%v), the documents say %d", len(got), got, len(want))
	}
	if total != 120 {
		t.Errorf("the matrix enumerates %d cases, the documents say 120", total)
	}
	for _, name := range []string{"README.md", "docs/distributed.md", "docs/guide/distributed-runtime.md"} {
		raw, err := os.ReadFile(filepath.Join(filepath.FromSlash(claimsRepo), filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if text := strings.Join(strings.Fields(string(raw)), " "); !strings.Contains(text, fmt.Sprintf("%d cases", total)) {
			t.Errorf("%s does not quote the matrix as %d cases", name, total)
		}
	}
}
