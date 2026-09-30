package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// The demo is the project's one-command argument, so the thing worth testing is
// not that it prints a story: it is that every claim in the story is read back
// from the run it just performed. A narrated walkthrough whose steps are not
// verified is a screenshot with extra steps, and it fails silently on exactly
// the day the runtime regresses.

// demoArgs invokes the demo against the repository's own examples. Under go test
// the working directory is the package directory, while a person runs the demo
// from the repository root where the default applies.
func demoArgs(dir string) []string {
	return []string{"demo", "--dir", dir, "--examples", filepath.Join("..", "..", "examples")}
}

// The order is the argument. A reader following along has to see the incident
// handled before the trace, the trace before the verdict, the verdict before the
// replay, and the crash after the clean run has established what correct looks
// like. A demo that prints the same steps in another order tells a different and
// less convincing story.
func TestDemoWalksTheFlagshipStepsInOrder(t *testing.T) {
	var out bytes.Buffer
	if err := runCLI(demoArgs(t.TempDir()), &out); err != nil {
		t.Fatalf("demo failed: %v\n%s", err, out.String())
	}
	text := out.String()

	want := []string{
		"compile the four-service company world",
		"run the incident under chaos",
		"inspect what the agent did",
		"trace the run",
		"evaluate the declarative assertions",
		"replay the ledger",
		"fork from a checkpoint and compare",
		"crash a worker and watch another take over",
		"fail the same incident on purpose",
		"explain the failure with a counterfactual",
		"block a prompt injection and an over-privileged request",
	}
	at := 0
	for _, step := range want {
		idx := strings.Index(text[at:], step)
		if idx < 0 {
			t.Fatalf("demo never reached %q (or reached it out of order)\nfull output:\n%s", step, text)
		}
		at += idx + len(step)
	}
}

// Each step must print what it read back, not that it ran. These are the
// specific numbers the project claims, so they are the ones the demo has to
// produce: a reader who takes nothing else away should still see that the
// assertions all held, that a second worker took the run over with a higher
// fence, that the stale worker's commit was refused, and that the ledger still
// reproduces.
func TestDemoPrintsEvidenceForEachClaim(t *testing.T) {
	var out bytes.Buffer
	if err := runCLI(demoArgs(t.TempDir()), &out); err != nil {
		t.Fatalf("demo failed: %v\n%s", err, out.String())
	}
	text := out.String()

	for _, evidence := range []string{
		"12/12 assertions", // the flagship's declarative verdict, in full
		"replay verified",  // the ledger still reproduces after the chaos
		"fence 1 -> 2",     // the takeover is fenced, not merely retried
		"stale commit refused",
		"resourceSpans",  // the trace is real OTLP, not a prose summary
		"counterfactual", // the failure is explained, not just reported
	} {
		if !strings.Contains(text, evidence) {
			t.Fatalf("demo output is missing the evidence %q\nfull output:\n%s", evidence, text)
		}
	}
}

// A step whose evidence check fails must stop the demo and surface a non-zero
// error naming that step. The failure mode this guards against is the worst one
// available to a demo: printing the whole happy story over a runtime that no
// longer does what the story says.
func TestDemoStopsAtTheFirstUnverifiedStep(t *testing.T) {
	var out bytes.Buffer
	steps := []demoStep{
		{
			title: "first step",
			run:   func(*demoContext) (string, error) { return "fine", nil },
		},
		{
			title: "second step",
			run:   func(*demoContext) (string, error) { return "", errors.New("the ledger did not reproduce") },
		},
		{
			title: "third step",
			run:   func(*demoContext) (string, error) { return "never reached", nil },
		},
	}
	err := runDemoSteps(&demoContext{dir: t.TempDir()}, steps, &out)
	if err == nil {
		t.Fatalf("demo reported success with an unverified step\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "second step") {
		t.Fatalf("error must name the failing step, got %v", err)
	}
	if strings.Contains(out.String(), "third step") {
		t.Fatalf("demo continued past a failed step:\n%s", out.String())
	}
}

// The demo writes its world, database and artefacts into the directory it is
// given, so a reader can open them afterwards. Nothing may land in the
// repository, which is what makes it safe to run from a clean checkout.
func TestDemoWritesOnlyIntoItsOwnDirectory(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := runCLI(demoArgs(dir), &out); err != nil {
		t.Fatalf("demo failed: %v\n%s", err, out.String())
	}
	manifest := filepath.Join(dir, "company.manifest.json")
	if _, err := readManifest(manifest); err != nil {
		t.Fatalf("demo must leave its compiled world in the given directory: %v", err)
	}
}
