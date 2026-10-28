package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// docs/claims.md is the list of everything the README says is true, with the
// test or CI job that holds each line. A list like that rots in a specific way:
// a test is renamed or deleted, a workflow job is dropped, and the row keeps
// citing something that no longer exists, so the document keeps claiming a proof
// nobody can run. These tests hold the part that can be checked by a machine:
// that what a row cites exists, that every row cites something or says plainly
// that nothing proves it, and that the documents a reader follows from the README
// do not point at files that are not there.
//
// What they do not hold: that a cited test is the right test for its claim, or
// that a cited CI job passed. Both are judgements a reader makes from the row.

const (
	claimsDocPath = "../../docs/claims.md"
	claimsRepo    = "../.."
	// The design spec has seven goals and six milestone sections, and the README
	// adds claims of its own, so a table shorter than this is not the table.
	minimumClaimRows = 20
)

var (
	claimToken    = regexp.MustCompile("`([^`\n]+)`")
	claimTestName = regexp.MustCompile(`^Test[A-Za-z0-9_]+$`)
	claimJobName  = regexp.MustCompile(`^([A-Za-z0-9_.-]+\.ya?ml)/([A-Za-z0-9_-]+)$`)
	claimEnvVar   = regexp.MustCompile(`TWINWRIGHT_TEST_[A-Z_]+`)
	claimTestDecl = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	claimMDLink   = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	// Whole words only: "cut" is also inside "execute".
	claimDeclared  = regexp.MustCompile(`\b(not verified|cut)\b`)
	claimHeader    = []string{"Claim", "Proven by", "Where it runs"}
	claimWhereKind = []string{"local suite", "ci fast lane", "integration", "opt-in", "local run", "none"}
)

// repositoryTests collects every Go test function name in the repository,
// skipping version control and the private working directory, which hold copies
// and scratch files that are not part of what a reader can run.
func repositoryTests(t *testing.T) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	err := filepath.WalkDir(filepath.FromSlash(claimsRepo), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == ".superpowers" {
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
func workflowJobs(t *testing.T, name string) (map[string]bool, error) {
	t.Helper()
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

// claimRows returns the data rows of the one table in the document, each as its
// three cells. A second header, a row with another number of cells, or no header
// at all is an error, because a table that cannot be parsed cannot be checked.
func claimRows(t *testing.T, text string) [][]string {
	t.Helper()
	var rows [][]string
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
					t.Errorf("%s line %d: column %d is %q, want %q", claimsDocPath, number+1, i+1, cells[i], want)
				}
			}
			continue
		}
		if len(cells) != len(claimHeader) {
			t.Errorf("%s line %d: a row has %d cells, want %d: %q", claimsDocPath, number+1, len(cells), len(claimHeader), line)
			continue
		}
		rows = append(rows, cells)
	}
	if headers != 1 {
		t.Fatalf("%s has %d header rows, want exactly 1 (%s)", claimsDocPath, headers, strings.Join(claimHeader, " | "))
	}
	return rows
}

func backticked(text string) []string {
	var out []string
	for _, m := range claimToken.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestClaimsTableReferencesExist(t *testing.T) {
	text := claimsDocument(t)
	tests := repositoryTests(t)

	// Everything backticked anywhere in the document is held, not only the cells
	// of the table, so a test named in a sentence under the table is checked too.
	checkedTests, checkedJobs := 0, 0
	jobsByFile := map[string]map[string]bool{}
	for _, token := range backticked(text) {
		switch {
		case claimTestName.MatchString(token):
			checkedTests++
			if !tests[token] {
				t.Errorf("%s cites %s, which is not declared as func %s( in any _test.go file", claimsDocPath, token, token)
			}
		case claimJobName.MatchString(token):
			checkedJobs++
			parts := claimJobName.FindStringSubmatch(token)
			file, job := parts[1], parts[2]
			jobs, seen := jobsByFile[file]
			if !seen {
				var err error
				jobs, err = workflowJobs(t, file)
				if err != nil {
					t.Errorf("%s cites %s, but .github/workflows/%s cannot be read as a workflow: %v", claimsDocPath, token, file, err)
				}
				jobsByFile[file] = jobs
			}
			if jobs != nil && !jobs[job] {
				known := make([]string, 0, len(jobs))
				for id := range jobs {
					known = append(known, id)
				}
				sort.Strings(known)
				t.Errorf("%s cites %s, but %s has no job %q (jobs: %s)", claimsDocPath, token, file, job, strings.Join(known, ", "))
			}
		}
	}
	if checkedTests == 0 || checkedJobs == 0 {
		t.Errorf("%s cites %d tests and %d CI jobs: a claims table that names neither kind of proof proves nothing", claimsDocPath, checkedTests, checkedJobs)
	}

	rows := claimRows(t, text)
	if len(rows) < minimumClaimRows {
		t.Errorf("%s has %d claim rows, want at least %d", claimsDocPath, len(rows), minimumClaimRows)
	}
	for _, row := range rows {
		claim, proof, where := row[0], row[1], row[2]
		if claim == "" || proof == "" || where == "" {
			t.Errorf("a row has an empty cell: %q", row)
			continue
		}
		cited := false
		var viaCI, viaIntegration bool
		for _, token := range backticked(proof) {
			if claimTestName.MatchString(token) {
				cited = true
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
			t.Errorf("row %q cites no test or CI job and does not say \"not verified\" or \"cut\"", claim)
		}

		lowerWhere := strings.ToLower(where)
		kind := false
		for _, name := range claimWhereKind {
			kind = kind || strings.Contains(lowerWhere, name)
		}
		if !kind {
			t.Errorf("row %q: %q is none of %s", claim, where, strings.Join(claimWhereKind, ", "))
		}
		if strings.Contains(lowerWhere, "opt-in") && !claimEnvVar.MatchString(where) {
			t.Errorf("row %q runs opt-in but names no TWINWRIGHT_TEST_ environment variable: %q", claim, where)
		}
		if strings.Contains(lowerWhere, "local run") && !strings.Contains(where, ".md") {
			t.Errorf("row %q is a local run but names no document: %q", claim, where)
		}
		if viaCI && !strings.Contains(lowerWhere, "ci fast lane") {
			t.Errorf("row %q cites a ci.yml job but does not say it runs in the CI fast lane: %q", claim, where)
		}
		if viaIntegration && !strings.Contains(lowerWhere, "integration") {
			t.Errorf("row %q cites an integration.yml job but does not say it runs in Integration: %q", claim, where)
		}
	}
}

// A relative link that points at a file that is not there is the commonest way a
// restructured document set breaks, and the README's rewrite moves a thousand
// lines of content into other files. Every link a reader can follow from the
// README, the architecture page, the claims table and the guide must land.
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
		inFence := false
		for number, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			for _, m := range claimMDLink.FindAllStringSubmatch(line, -1) {
				target, _, _ := strings.Cut(m[1], "#")
				if target == "" ||
					strings.HasPrefix(target, "http://") ||
					strings.HasPrefix(target, "https://") ||
					strings.HasPrefix(target, "mailto:") {
					continue
				}
				checked++
				resolved := filepath.Join(filepath.Dir(path), filepath.FromSlash(target))
				if _, err := os.Stat(resolved); err != nil {
					t.Errorf("%s line %d links to %s, which does not exist", filepath.ToSlash(path), number+1, m[1])
				}
			}
		}
	}
	if len(files) > 0 && checked == 0 {
		t.Error("no relative link was checked, so this test would pass on documents that link to nothing")
	}
}
