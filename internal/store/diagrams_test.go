package store

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The distributed diagrams live in docs, but their tests live here, beside the
// schema they describe. A diagram is a claim about this package: rename a table
// in migrate.go and the failure should land in the same package the renamer is
// already editing, not in a docs suite nobody runs.

const distributedDoc = "../../docs/distributed.md"

// snakeIdentifier matches the shape a database identifier takes in these
// diagrams. Prose uses hyphens (at-least-once, take-over-on-expiry), so a
// match here is a claim about the schema rather than an English phrase.
var snakeIdentifier = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\b`)

var mermaidArrow = regexp.MustCompile(`^\s*(\w+)\s*(?:-->>|->>|-->|->|--x|-x|-\))\s*(\w+)\s*:`)

var mermaidNote = regexp.MustCompile(`^\s*Note\s+(?:over|left of|right of)\s+([\w\s,]+?)\s*:`)

// mermaidBlocks returns the body of every fenced mermaid block in the file.
func mermaidBlocks(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var blocks []string
	var current []string
	inside := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimRight(line, "\r")
		switch {
		case !inside && strings.HasPrefix(strings.TrimSpace(trimmed), "```mermaid"):
			inside = true
			current = nil
		case inside && strings.TrimSpace(trimmed) == "```":
			inside = false
			blocks = append(blocks, strings.Join(current, "\n"))
		case inside:
			current = append(current, trimmed)
		}
	}
	if inside {
		t.Fatalf("%s has an unterminated mermaid block", path)
	}
	return blocks
}

// schemaIdentifiers collects every identifier the migration DDL declares:
// table names and column names alike. A diagram may name any of them and
// nothing else.
func schemaIdentifiers(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile("migrate.go")
	if err != nil {
		t.Fatalf("read migrate.go: %v", err)
	}
	found := map[string]bool{}
	for _, id := range snakeIdentifier.FindAllString(string(raw), -1) {
		found[id] = true
	}
	if len(found) == 0 {
		t.Fatal("migrate.go declared no identifiers, so this test cannot detect drift")
	}
	return found
}

func TestDistributedDiagramsExist(t *testing.T) {
	blocks := mermaidBlocks(t, distributedDoc)
	if len(blocks) < 3 {
		t.Fatalf("want at least 3 mermaid diagrams in %s, got %d", distributedDoc, len(blocks))
	}
	wantKinds := map[string]bool{
		"flowchart":       false,
		"sequenceDiagram": false,
		"stateDiagram-v2": false,
	}
	for _, block := range blocks {
		header := ""
		for _, line := range strings.Split(block, "\n") {
			if strings.TrimSpace(line) != "" {
				header = strings.TrimSpace(line)
				break
			}
		}
		matched := false
		for kind := range wantKinds {
			if strings.HasPrefix(header, kind) {
				wantKinds[kind] = true
				matched = true
			}
		}
		if !matched {
			t.Errorf("diagram header %q is not a recognised mermaid diagram type", header)
		}
	}
	for kind, present := range wantKinds {
		if !present {
			t.Errorf("no %s diagram: the distributed story needs topology, the takeover sequence, and the lease lifecycle", kind)
		}
	}
}

// A sequence diagram whose messages name an undeclared participant renders as
// an extra lifeline with a bare identifier for a label, which reads as a
// different component than the one meant.
func TestDistributedSequenceParticipantsAreDeclared(t *testing.T) {
	for i, block := range mermaidBlocks(t, distributedDoc) {
		lines := strings.Split(block, "\n")
		if len(lines) == 0 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "sequenceDiagram") {
			continue
		}
		declared := map[string]bool{}
		for _, line := range lines {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) >= 2 && (fields[0] == "participant" || fields[0] == "actor") {
				declared[fields[1]] = true
			}
		}
		if len(declared) == 0 {
			t.Errorf("diagram %d declares no participants", i)
		}
		used := map[string]bool{}
		for _, line := range lines {
			if m := mermaidArrow.FindStringSubmatch(line); m != nil {
				used[m[1]] = true
				used[m[2]] = true
			}
			if m := mermaidNote.FindStringSubmatch(line); m != nil {
				for _, part := range strings.Split(m[1], ",") {
					if p := strings.TrimSpace(part); p != "" {
						used[p] = true
					}
				}
			}
		}
		if len(used) == 0 {
			t.Errorf("diagram %d has participants but no messages between them", i)
		}
		for _, name := range sortedKeys(used) {
			if !declared[name] {
				t.Errorf("diagram %d references participant %q without declaring it", i, name)
			}
		}
	}
}

// The drift guard. Every database identifier the diagrams name must exist in
// the migration DDL, so renaming a table or column in this package breaks the
// diagram that describes it.
func TestDistributedDiagramsNameOnlyRealSchemaIdentifiers(t *testing.T) {
	schema := schemaIdentifiers(t)
	// Terms that look like identifiers but describe the runtime rather than a
	// column: mermaid keywords and CLI/flag spellings.
	allowed := map[string]bool{
		"stateDiagram_v2": true,
		"skip_locked":     true,
		"for_update":      true,
	}
	for i, block := range mermaidBlocks(t, distributedDoc) {
		for _, id := range snakeIdentifier.FindAllString(block, -1) {
			if schema[id] || allowed[id] {
				continue
			}
			t.Errorf("diagram %d names %q, which is not in the schema: a diagram must not describe a table or column that does not exist", i, id)
		}
	}
}

// The four tables the distributed runtime added are the subject of these
// diagrams. A diagram set that omits one of them is describing something else.
func TestDistributedDiagramsCoverTheDistributedTables(t *testing.T) {
	blocks := strings.Join(mermaidBlocks(t, distributedDoc), "\n")
	for _, table := range []string{"work_queue", "run_leases", "run_ownership_log", "workers"} {
		if !strings.Contains(blocks, table) {
			t.Errorf("no diagram names %s", table)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
