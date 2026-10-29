package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// scripts/demo-stack.sh needs Docker, so these tests cannot run it. They run what
// can run without it: the script against a stand-in docker for the part before
// the stack starts, and reads of the script, the Compose file, the Makefile and
// the workflow for the contracts between them. The full run is the demo-stack
// job and `make demo-stack`.

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// functionBody returns the text of a top-level shell function, from its opening
// line to the closing brace at the start of a line.
func functionBody(t *testing.T, script, name string) string {
	t.Helper()
	start := strings.Index(script, "\n"+name+"() {")
	if start < 0 {
		t.Fatalf("the script has no function %s", name)
	}
	end := strings.Index(script[start+1:], "\n}")
	if end < 0 {
		t.Fatalf("function %s has no closing brace", name)
	}
	return script[start : start+1+end+2]
}

// A worker left over from an earlier run holds a metrics port and can reconnect
// to the database the script recreates, and a process that happens to hold the
// port would answer the Prometheus checks in the worker's place. The script has
// to refuse an occupied port before it starts anything, and a failed run keeps
// its work directory and prints where it is.
func TestDemoStackRefusesAnOccupiedMetricsPortBeforeStartingAnything(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the script is bash")
	}
	for _, tool := range []string{"bash", "curl", "jq", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH, and the script requires it", tool)
		}
	}
	// The script's worker A listens on this port. Holding it here is the same
	// situation as a leftover worker.
	listener, err := net.Listen("tcp", "127.0.0.1:9095")
	if err != nil {
		t.Skipf("port 9095 is not free on this machine: %v", err)
	}
	defer listener.Close()

	bin := t.TempDir()
	calls := filepath.Join(bin, "docker-calls.log")
	stub := "#!/bin/sh\necho \"$*\" >> '" + calls + "'\n" +
		"if [ \"$1 $2\" = \"compose version\" ]; then echo 'Docker Compose version stub'; exit 0; fi\nexit 1\n"
	if err = os.WriteFile(filepath.Join(bin, "docker"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()

	cmd := exec.Command("bash", filepath.FromSlash("../../scripts/demo-stack.sh"))
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+tmp)
	output, err := cmd.CombinedOutput()
	text := string(output)

	var exitErr *exec.ExitError
	if err == nil {
		t.Fatalf("the script ran on with port 9095 occupied:\n%s", text)
	}
	if ok := asExitError(err, &exitErr); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("exit = %v, want 1 (a failed check):\n%s", err, text)
	}
	if !strings.Contains(text, "9095") || !strings.Contains(text, "in use") {
		t.Errorf("the failure does not name the port and say it is in use:\n%s", text)
	}
	if log, _ := os.ReadFile(calls); strings.Contains(string(log), "up") {
		t.Errorf("the script started the stack before it checked the port; docker was called with:\n%s", log)
	}

	// A failed run keeps its work directory and says where it is.
	match := regexp.MustCompile(`Work directory: (\S+)`).FindStringSubmatch(text)
	if match == nil {
		t.Fatalf("a failed run does not print its work directory:\n%s", text)
	}
	if _, err := os.Stat(match[1]); err != nil {
		t.Errorf("the work directory %s was not kept after a failure: %v", match[1], err)
	}
}

func asExitError(err error, target **exec.ExitError) bool {
	exitErr, ok := err.(*exec.ExitError)
	if ok {
		*target = exitErr
	}
	return ok
}

// docker-compose.yml tells the reader to change the host port if they already run
// PostgreSQL, and the script used to connect to 5432 whatever they changed. Both
// read one variable with one default, so changing it changes both.
func TestDemoStackPostgresPortComesFromOneVariable(t *testing.T) {
	compose := readRepoFile(t, "../../docker-compose.yml")
	if !regexp.MustCompile(`(?m)^\s*- "127\.0\.0\.1:\$\{TW_DEMO_PG_PORT:-5432\}:5432"\s*$`).MatchString(compose) {
		t.Error(`docker-compose.yml does not publish PostgreSQL on "127.0.0.1:${TW_DEMO_PG_PORT:-5432}:5432"`)
	}
	script := readRepoFile(t, "../../scripts/demo-stack.sh")
	if !strings.Contains(script, "TW_DEMO_PG_PORT:-5432") {
		t.Error("scripts/demo-stack.sh does not read TW_DEMO_PG_PORT with the default 5432")
	}
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") || !strings.Contains(line, "postgres://") {
			continue
		}
		if strings.Contains(line, ":5432") {
			t.Errorf("the script builds a DSN with the port written in: %s", strings.TrimSpace(line))
		}
	}
}

// A failure message that says "see <file>" is only useful if the file survives
// the runner. The script copies a set of files to TW_DEMO_LOGS, and the workflow
// has to show what was copied.
func TestDemoStackSavesAndShowsEveryFileItsFailuresPointTo(t *testing.T) {
	script := readRepoFile(t, "../../scripts/demo-stack.sh")
	saved := functionBody(t, script, "save_logs")

	named := map[string]bool{"run-statuses.txt": true, "enqueue.err": true, "bench.out": true}
	mention := regexp.MustCompile(`\$WORK/([A-Za-z0-9._-]+)`)
	for _, line := range strings.Split(script, "\n") {
		if !strings.Contains(line, "evidence 1") {
			continue
		}
		for _, m := range mention.FindAllStringSubmatch(line, -1) {
			named[m[1]] = true
		}
	}
	for file := range named {
		if !strings.Contains(saved, file) {
			t.Errorf("a failure message points to %s and save_logs does not copy it", file)
		}
	}

	workflow := readRepoFile(t, "../../.github/workflows/integration.yml")
	job := workflow[strings.Index(workflow, "\n  demo-stack:"):]
	if next := regexp.MustCompile(`\n  [a-z][a-z-]*:\n`).FindStringIndex(job[len("\n  demo-stack:"):]); next != nil {
		job = job[:len("\n  demo-stack:")+next[0]]
	}
	if !strings.Contains(job, `"$TW_DEMO_LOGS"/*`) {
		t.Error(`the demo-stack job does not print every file in "$TW_DEMO_LOGS"`)
	}
}

// Both demos used to leave a temporary directory behind on every run, and the
// demo-stack one holds a built binary. A run that succeeds removes it; a run that
// fails keeps it, because its logs are the evidence, and says where it is.
func TestDemoStackRemovesItsWorkDirectoryOnlyOnSuccess(t *testing.T) {
	script := readRepoFile(t, "../../scripts/demo-stack.sh")
	cleanup := functionBody(t, script, "cleanup")
	if !strings.Contains(cleanup, `rm -rf "$WORK"`) {
		t.Fatal("the exit trap never removes the work directory")
	}
	removal := regexp.MustCompile(`(?s)if \[ "\$rc" -eq 0 \]; then[^\n]*\n[^\n]*rm -rf "\$WORK"`)
	if !removal.MatchString(cleanup) {
		t.Errorf("the removal is not conditional on a zero exit status:\n%s", cleanup)
	}
	if !strings.Contains(cleanup, "Work directory:") {
		t.Error("a failed run does not print its work directory")
	}
}

func TestMakeDemoRemovesItsDirectoryOnlyOnSuccess(t *testing.T) {
	_, recipe := makeRecipe(t, "demo")
	command := strings.Join(recipe, " ")
	for _, want := range []string{"mktemp -d", "rm -rf", "kept"} {
		if !strings.Contains(command, want) {
			t.Errorf("make demo does not contain %q: %s", want, command)
		}
	}
	if !regexp.MustCompile(`if \./twinwright demo --dir [^;]*; then rm -rf`).MatchString(command) {
		t.Errorf("make demo does not remove the directory only when the demo succeeds: %s", command)
	}
}
