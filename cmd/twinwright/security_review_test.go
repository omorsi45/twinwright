package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A security review is only worth the citations in it. Prose decays silently: a
// test gets renamed, a guard moves, and the document keeps saying the property is
// held by something that no longer exists. These checks hold the mechanical part
// of that - that every file and every test the review names is real - so the
// document fails the build instead of quietly becoming fiction.
//
// What they cannot check is whether a cited test asserts what the review says it
// asserts. That is why the review quotes the assertion rather than only naming the
// test, and why a reader is pointed at the test rather than asked to take the
// sentence on trust.

const securityReviewPath = "../../docs/security-review.md"

var (
	citedPath = regexp.MustCompile("`([A-Za-z0-9_./-]+\\.(?:go|md|yaml|yml))`")
	citedTest = regexp.MustCompile("`(Test[A-Za-z0-9_]+)`")
)

func securityReview(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(securityReviewPath))
	if err != nil {
		t.Fatalf("read %s: %v", securityReviewPath, err)
	}
	if len(raw) == 0 {
		t.Fatalf("%s is empty, so these checks would pass on nothing", securityReviewPath)
	}
	return string(raw)
}

// A citation to a file that does not exist sends a reader looking for the guard
// behind a claim and hands them nothing.
func TestSecurityReviewCitesFilesThatExist(t *testing.T) {
	text := securityReview(t)
	matches := citedPath.FindAllStringSubmatch(text, -1)
	if len(matches) < 10 {
		t.Fatalf("%s cites %d files; a review that cites almost nothing would pass this check on nothing", securityReviewPath, len(matches))
	}
	seen := map[string]bool{}
	for _, m := range matches {
		path := m[1]
		if seen[path] {
			continue
		}
		seen[path] = true
		if _, err := os.Stat(filepath.FromSlash("../../" + path)); err != nil {
			t.Errorf("the review cites %s, which is not a file in this repository", path)
		}
	}
}

// A citation to a test is the load-bearing kind: it is the difference between a
// claim and evidence.
func TestSecurityReviewCitesTestsThatExist(t *testing.T) {
	text := securityReview(t)
	matches := citedTest.FindAllStringSubmatch(text, -1)
	if len(matches) < 10 {
		t.Fatalf("%s cites %d tests; a review whose claims rest on nothing is the thing this check exists to catch", securityReviewPath, len(matches))
	}
	defined := definedTests(t)
	if len(defined) == 0 {
		t.Fatal("found no test functions in the repository, so this check would pass on nothing")
	}
	seen := map[string]bool{}
	for _, m := range matches {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if !defined[name] {
			t.Errorf("the review cites %s, which is not a test function in this repository", name)
		}
	}
}

// Every surface the review is supposed to cover has to have a section of its own.
// Dropping one is the quiet failure: the document still reads as a complete
// review. Looking for the word anywhere in the text is not enough - "container"
// appears in half a dozen sentences - so the check is against the headings.
func TestSecurityReviewCoversEverySurface(t *testing.T) {
	var headings []string
	for _, line := range strings.Split(securityReview(t), "\n") {
		if strings.HasPrefix(line, "## ") {
			headings = append(headings, strings.ToLower(line))
		}
	}
	if len(headings) < 7 {
		t.Fatalf("the review has %d sections; it is supposed to cover seven surfaces", len(headings))
	}
	for _, surface := range []struct{ name, inHeading string }{
		{"the authorization boundary", "authorization"},
		{"prompt injection", "prompt injection"},
		{"secret redaction", "redaction"},
		{"the container surface", "container"},
		{"the shadow connector", "shadow"},
		{"SQL construction", "sql"},
		{"what is deliberately not covered", "deliberately"},
	} {
		found := false
		for _, heading := range headings {
			if strings.Contains(heading, surface.inHeading) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no section of the review covers %s", surface.name)
		}
	}
}

// definedTests collects every Go test function name in the repository.
func definedTests(t *testing.T) map[string]bool {
	t.Helper()
	declaration := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	out := map[string]bool{}
	err := filepath.WalkDir(filepath.FromSlash("../.."), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
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
		for _, m := range declaration.FindAllStringSubmatch(string(raw), -1) {
			out[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
