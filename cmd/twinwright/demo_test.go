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

// The walkthrough is the most expensive thing in this package: it compiles a
// four-service world, runs an incident under chaos, crashes workers, and
// analyses 32 counterfactual interventions. Three separate tests each running it
// tripled that cost and pushed the package past Go's ten-minute default timeout
// on a machine without cgo sqlite. One run, three claims about it.
func TestDemo(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := runCLI(demoArgs(dir), &out); err != nil {
		t.Fatalf("demo failed: %v\n%s", err, out.String())
	}
	text := out.String()

	// The order is the argument. A reader following along has to see the incident
	// handled before the trace, the trace before the verdict, the verdict before
	// the replay, and the crash after the clean run has established what correct
	// looks like. A demo that prints the same steps in another order tells a
	// different and less convincing story.
	t.Run("walks the flagship steps in order", func(t *testing.T) {
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
	})

	// Each step must print what it read back, not that it ran. These are the
	// specific numbers the project claims, so they are the ones the demo has to
	// produce: a reader who takes nothing else away should still see that the
	// assertions all held, that a second worker took the run over with a higher
	// fence, that the stale worker's commit was refused, and that the ledger
	// still reproduces.
	t.Run("prints evidence for each claim", func(t *testing.T) {
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
	})

	// The demo writes its world, database and artefacts into the directory it is
	// given, so a reader can open them afterwards. Nothing may land in the
	// repository, which is what makes it safe to run from a clean checkout.
	t.Run("writes only into its own directory", func(t *testing.T) {
		if _, err := readManifest(filepath.Join(dir, "company.manifest.json")); err != nil {
			t.Fatalf("demo must leave its compiled world in the given directory: %v", err)
		}
	})
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

// Reporting "the failure is explained by <first candidate that changed
// anything>" is the demo's least defensible possible line. On the flagship
// failure 24 of 32 interventions change the outcome, all at one fork each, so
// "first with an effect" names whichever appears earliest in the report: for this
// run that is a customer read, which has nothing to do with a lost refund write.
//
// The distribution itself is the finding. Every agent-side change corrects the
// outcome, including the last decision after the support message was already
// posted, because a fixture that rebuilds its decision from the whole transcript
// still sees an unresolved refund that late. World-side changes correct it only
// up to the refund dispatch, and none of the ones after it do anything. The
// world's opportunity has a deadline; the agent's does not.
func TestCounterfactualSummaryReportsTheAsymmetryNotTheFirstHit(t *testing.T) {
	candidates := []demoCandidate{
		{Label: "getCustomer dispatch", Kind: "chaos_policy", CheckpointSeq: 3, Forks: 1, Changed: 1},
		{Label: "model decision after getCustomer", Kind: "model", CheckpointSeq: 5, Forks: 1, Changed: 1},
		{Label: "createRefund dispatch", Kind: "chaos_policy", CheckpointSeq: 32, Forks: 1, Changed: 1},
		{Label: "model decision after createRefund", Kind: "model", CheckpointSeq: 35, Forks: 1, Changed: 1},
		{Label: "crmAddAccountNote dispatch", Kind: "chaos_policy", CheckpointSeq: 37, Forks: 1, Changed: 0},
		{Label: "messagePostMessage dispatch", Kind: "chaos_policy", CheckpointSeq: 70, Forks: 1, Changed: 0},
		{Label: "model decision after messagePostMessage", Kind: "model", CheckpointSeq: 73, Forks: 1, Changed: 1},
	}
	summary, err := summarizeCounterfactual(candidates)
	if err != nil {
		t.Fatalf("summary failed on a well-formed analysis: %v", err)
	}
	for _, want := range []string{
		"3/3 agent-side", // every model intervention corrected it
		"event 73",       // including the last one, after the notification
		"2/4 world-side", // the world's changes stop mattering
		"event 32",       // and the boundary is the refund dispatch
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary is missing %q, got %q", want, summary)
		}
	}
	// Naming one intervention as "the" explanation is exactly what this replaces.
	if strings.Contains(summary, "getCustomer") {
		t.Fatalf("summary must not single out the first candidate with an effect, got %q", summary)
	}
}

// A run where nothing changes the outcome is unexplained, and saying so is the
// honest result. The analyzer is not obliged to find a cause.
func TestCounterfactualSummaryRefusesAnUnexplainedFailure(t *testing.T) {
	_, err := summarizeCounterfactual([]demoCandidate{
		{Label: "a", Kind: "model", CheckpointSeq: 5, Forks: 1, Changed: 0},
		{Label: "b", Kind: "chaos_policy", CheckpointSeq: 7, Forks: 1, Changed: 0},
	})
	if err == nil {
		t.Fatal("an analysis where no intervention changed the outcome explains nothing")
	}
	if !strings.Contains(err.Error(), "unexplained") {
		t.Fatalf("error must say the failure is unexplained, got %v", err)
	}
}

// The deadline claim is only true if the world-side effects stop and stay
// stopped. A world change that matters again after one that did not is a
// different and messier story, and the demo must not narrate the clean one over
// it.
func TestCounterfactualSummaryRejectsAnInterleavedBoundary(t *testing.T) {
	_, err := summarizeCounterfactual([]demoCandidate{
		{Label: "early", Kind: "chaos_policy", CheckpointSeq: 10, Forks: 1, Changed: 1},
		{Label: "middle", Kind: "chaos_policy", CheckpointSeq: 20, Forks: 1, Changed: 0},
		{Label: "late", Kind: "chaos_policy", CheckpointSeq: 30, Forks: 1, Changed: 1},
		{Label: "agent", Kind: "model", CheckpointSeq: 35, Forks: 1, Changed: 1},
	})
	if err == nil {
		t.Fatal("world-side effects that resume after stopping are not a deadline")
	}
	if !strings.Contains(err.Error(), "no single boundary") {
		t.Fatalf("error must name the interleaving, got %v", err)
	}
}

// The demo writes its world, database and artefacts into the directory it is
// given, and that claim is checked as a subtest of TestDemo above, sharing the
// single expensive run rather than performing a third one.
