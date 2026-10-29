package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// scriptCommands returns the shell commands of a script with backslash
// continuations joined, so a command that spans lines can be read as one.
func scriptCommands(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	var commands []string
	var current strings.Builder
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasSuffix(trimmed, "\\") {
			current.WriteString(strings.TrimSuffix(trimmed, "\\") + " ")
			continue
		}
		current.WriteString(trimmed)
		if current.Len() > 0 {
			commands = append(commands, current.String())
		}
		current.Reset()
	}
	return commands
}

// "run" exits 0 when the evaluation fails and prints a "passed" for every check,
// so grepping its output for "passed":true matches a run in which one check
// passed and the others failed. The two runs e2e.sh expects to pass must ask the
// command to fail instead; the unsafe run is expected to fail and must not.
func TestE2EScriptFailsWhenAnExpectedlyPassingRunFailsItsEvaluation(t *testing.T) {
	commands := scriptCommands(t, "../../scripts/e2e.sh")
	expectPass := []struct{ name, marker string }{
		{"the duplicate-charge run", `"$BIN" run duplicate-charge`},
		{"the safe ambiguous-commit run", `--recovery safe`},
	}
	for _, want := range expectPass {
		found := false
		for _, command := range commands {
			if !strings.Contains(command, want.marker) || !strings.Contains(command, `"$BIN" run `) || strings.Contains(command, "--enqueue") {
				continue
			}
			found = true
			if !strings.Contains(command, "--fail-on-eval") {
				t.Errorf("%s does not pass --fail-on-eval, so a failing evaluation does not stop e2e.sh: %s", want.name, command)
			}
		}
		if !found {
			t.Errorf("e2e.sh has no command for %s", want.name)
		}
	}

	for _, command := range commands {
		if strings.Contains(command, "--recovery unsafe") && strings.Contains(command, "--fail-on-eval") {
			t.Errorf("the unsafe run is expected to fail its evaluation and must not pass --fail-on-eval: %s", command)
		}
	}

	// A grep of a run's output for a passing check is the evidence that proved
	// nothing. The assertions file's own grep, on the output of evaluate (which
	// exits 1 on a failing assertion), is a different command and stays.
	stale := regexp.MustCompile(`grep -q '"passed":true' "\$WORK/(run|safe)\.json"`)
	for _, command := range commands {
		if stale.MatchString(command) {
			t.Errorf("e2e.sh still greps a run's output for a passing check: %s", command)
		}
	}
}
