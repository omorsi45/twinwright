package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"twinwright/internal/store"
)

// A changelog is a set of claims about the build, and claims rot. These tests
// hold the ones that can be checked mechanically: that a release exists and is
// dated, that versions descend, that every ADR it cites is a file, and that
// every schema version it advertises is one this build actually writes.

const changelogPath = "../../CHANGELOG.md"

var (
	releaseHeading = regexp.MustCompile(`^## v(\d+)\.(\d+)\.(\d+) - (\d{4}-\d{2}-\d{2})$`)
	anyHeading     = regexp.MustCompile(`^## `)
	adrReference   = regexp.MustCompile(`ADR (\d{4})`)
	schemaClaim    = regexp.MustCompile(`schema version (\d+)`)
)

func changelog(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(changelogPath))
	if err != nil {
		t.Fatalf("read %s: %v", changelogPath, err)
	}
	if len(raw) == 0 {
		t.Fatalf("%s is empty, so these checks would pass on nothing", changelogPath)
	}
	return string(raw)
}

type release struct {
	major, minor, patch int
	date                string
	line                int
}

func releases(t *testing.T, text string) []release {
	t.Helper()
	var found []release
	for i, line := range strings.Split(text, "\n") {
		m := releaseHeading.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		patch, _ := strconv.Atoi(m[3])
		found = append(found, release{major: major, minor: minor, patch: patch, date: m[4], line: i + 1})
	}
	return found
}

func TestChangelogHasADatedRelease(t *testing.T) {
	text := changelog(t)
	found := releases(t, text)
	if len(found) == 0 {
		t.Fatalf("%s has no released version section: want a heading like \"## v0.1.0 - 2026-09-30\"", changelogPath)
	}
	// An Unreleased section is fine and expected, but it must not be the only
	// one, and a release section must carry entries rather than a bare heading.
	lines := strings.Split(text, "\n")
	first := found[0]
	body := 0
	for i := first.line; i < len(lines); i++ {
		if anyHeading.MatchString(lines[i]) {
			break
		}
		if strings.TrimSpace(lines[i]) != "" {
			body++
		}
	}
	if body == 0 {
		t.Errorf("release v%d.%d.%d has a heading and no entries", first.major, first.minor, first.patch)
	}
}

func TestChangelogReleasesAreNewestFirst(t *testing.T) {
	found := releases(t, changelog(t))
	for i := 1; i < len(found); i++ {
		prev, cur := found[i-1], found[i]
		if cur.major > prev.major ||
			(cur.major == prev.major && cur.minor > prev.minor) ||
			(cur.major == prev.major && cur.minor == prev.minor && cur.patch >= prev.patch) {
			t.Errorf("v%d.%d.%d at line %d is not older than v%d.%d.%d at line %d: the file says newest first",
				cur.major, cur.minor, cur.patch, cur.line, prev.major, prev.minor, prev.patch, prev.line)
		}
	}
}

// A citation to a decision record that no longer exists sends a reader looking
// for the reasoning behind a change and hands them nothing.
func TestChangelogAdrReferencesResolve(t *testing.T) {
	text := changelog(t)
	matches := adrReference.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		t.Fatal("the changelog cites no ADRs, so this check would pass on nothing")
	}
	seen := map[string]bool{}
	for _, m := range matches {
		number := m[1]
		if seen[number] {
			continue
		}
		seen[number] = true
		hits, err := filepath.Glob(filepath.FromSlash("../../docs/adr/" + number + "-*.md"))
		if err != nil {
			t.Fatalf("glob adr %s: %v", number, err)
		}
		if len(hits) == 0 {
			t.Errorf("the changelog cites ADR %s, which is not a file in docs/adr", number)
		}
	}
}

// The changelog advertises the schema version a release writes. A build that
// writes a different one makes the entry a lie in the direction that matters:
// an operator reads it to decide whether a database is compatible.
func TestChangelogSchemaClaimsMatchThisBuild(t *testing.T) {
	text := changelog(t)
	matches := schemaClaim.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		t.Fatal("the changelog claims no schema version, so this check would pass on nothing")
	}
	highest := 0
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("schema version %q is not a number: %v", m[1], err)
		}
		if n > highest {
			highest = n
		}
		if n > store.CurrentSchemaVersion {
			t.Errorf("the changelog claims schema version %d, but this build writes %d", n, store.CurrentSchemaVersion)
		}
	}
	if highest != store.CurrentSchemaVersion {
		t.Errorf("the changelog's newest schema claim is %d and this build writes %d: a migration landed without a changelog entry",
			highest, store.CurrentSchemaVersion)
	}
}
