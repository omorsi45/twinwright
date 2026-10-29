package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"twinwright/internal/agent"
	"twinwright/internal/bench"
	"twinwright/internal/gate"
)

// The narrated walkthrough of section 9. It exists because the repository's
// argument is only convincing when a reader sees it happen: an incident handled
// across four services under injected faults, audited, judged, replayed, forked,
// survived a worker crash, then deliberately failed and explained.
//
// Two decisions shape the whole file.
//
// Every step runs through runCLI, the same entry point a person types into a
// shell. The demo therefore executes the documented interface rather than a
// parallel implementation of it, so a flag that changes underneath breaks this
// command instead of quietly leaving the walkthrough describing a CLI that no
// longer exists. It also means the demo needs no shell, which matters because a
// script would not run on every platform the binary does.
//
// Every step reads its evidence back out of what it just ran, and the demo stops
// at the first step whose evidence is missing. A narration that prints a story
// without checking it is worse than no demo at all: it keeps telling the happy
// story on the day the runtime regresses, which is exactly the day someone is
// reading it to decide whether to trust the project.

// demoContext carries the paths the steps share. Later steps consume run IDs
// produced by earlier ones, which is the ordering the walkthrough depends on.
type demoContext struct {
	dir      string
	examples string
	// manifest is the compiled four-service company world.
	manifest string
	// db holds the healthy incident run, its fork and its lineage.
	db string
	// runID is the healthy incident. failedID is the same incident with the
	// refund write lost rather than merely unacknowledged, which is the run the
	// counterfactual explains.
	runID    string
	failedID string
	failedDB string
}

// demoStep is one narrated step. run returns the one-line evidence to print, or
// an error that stops the walkthrough. Returning the evidence rather than
// printing it is what lets runDemoSteps guarantee no step can report success
// without producing something read back from the system.
type demoStep struct {
	title string
	// why is the sentence a reader needs to understand what the step proves.
	why string
	run func(*demoContext) (string, error)
}

var runIDPattern = regexp.MustCompile(`R-[0-9a-f]{24}`)

func cmdDemo(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "directory for the demo's world, database and artefacts (required)")
	examples := fs.String("examples", "examples", "directory holding the example worlds, chaos policies and assertions")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*dir) == "" {
		return fmt.Errorf("demo requires --dir: it writes a world, a database and several artefacts, and must not scatter them into the working tree")
	}
	// A first run names a directory that does not exist yet, and the underlying
	// commands only open files: without this the demo fails on its first step
	// with an error about a manifest rather than about the directory.
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	ctx := &demoContext{
		dir:      *dir,
		examples: *examples,
		manifest: filepath.Join(*dir, "company.manifest.json"),
		db:       filepath.Join(*dir, "company.db"),
		failedDB: filepath.Join(*dir, "company-failed.db"),
	}
	return runDemoSteps(ctx, demoSteps(), out)
}

// runDemoSteps narrates the walkthrough and refuses to continue past a step
// whose evidence did not appear. The error names the step, because "the demo
// failed" is not actionable and "replay the ledger failed" is.
func runDemoSteps(ctx *demoContext, steps []demoStep, out io.Writer) error {
	fmt.Fprintf(out, "Twinwright: one incident, end to end\n")
	fmt.Fprintf(out, "%d steps, all scripted: no API key, no network, no container runtime.\n", len(steps))
	for i, step := range steps {
		fmt.Fprintf(out, "\n=== %d/%d %s\n", i+1, len(steps), step.title)
		if step.why != "" {
			fmt.Fprintf(out, "%s\n", step.why)
		}
		evidence, err := step.run(ctx)
		if err != nil {
			fmt.Fprintf(out, "FAILED: %v\n", err)
			return fmt.Errorf("demo stopped at step %d, %s: %w", i+1, step.title, err)
		}
		fmt.Fprintf(out, "evidence: %s\n", evidence)
	}
	fmt.Fprintf(out, "\nEvery step verified its own claim. The world, ledger and artefacts are in %s\n", ctx.dir)
	return nil
}

func demoSteps() []demoStep {
	return []demoStep{
		{
			title: "compile the four-service company world",
			why:   "Billing, CRM, ticketing and messaging, compiled from OpenAPI into one simulated world with a seeded database.",
			run:   demoBuildWorld,
		},
		{
			title: "run the incident under chaos",
			why:   "A duplicate charge to investigate across all four services, with billing timing out once and the refund's response lost after it commits.",
			run:   demoRunIncident,
		},
		{
			title: "inspect what the agent did",
			why:   "Every tool call and its outcome, read back from the ledger rather than from the agent's own account of itself.",
			run:   demoInspect,
		},
		{
			title: "trace the run",
			why:   "The same ledger rendered as OpenTelemetry spans, so an existing tracing stack can read it.",
			run:   demoTrace,
		},
		{
			title: "evaluate the declarative assertions",
			why:   "Twelve assertions written against the world's final state and the ledger's order, not against the agent's text.",
			run:   demoEvaluate,
		},
		{
			title: "replay the ledger",
			why:   "Re-executing the recorded decisions must reproduce the same world. A run that cannot be replayed cannot be audited.",
			run:   demoReplay,
		},
		{
			title: "fork from a checkpoint and compare",
			why:   "Branch the run at a committed boundary and diff the two worlds, which is how a counterfactual is grounded in state rather than in argument.",
			run:   demoForkAndCompare,
		},
		{
			title: "crash a worker and watch another take over",
			why:   "Kill the worker mid-incident, let its lease expire, and require the zombie's late commit to be refused by fencing.",
			run:   demoCrashAndTakeover,
		},
		{
			title: "fail the same incident on purpose",
			why:   "Same scenario, but the refund never commits and the fixture assumes it did. This is the failure the project exists to catch.",
			run:   demoFailOnPurpose,
		},
		{
			title: "explain the failure with a counterfactual",
			why:   "Re-run from each committed boundary with one thing changed, and report which change would have altered the outcome.",
			run:   demoCounterfactual,
		},
		{
			title: "block a prompt injection and an over-privileged request",
			why:   "A ticket body carrying instructions, and a principal asking for another customer's data. Both must be refused.",
			run:   demoSecurity,
		},
		{
			title: "block a regressed agent at the CI gate",
			why:   "The same suite with a fixture that investigates, then claims the issue is resolved without making the write. The gate compares it with the approved baseline and must refuse it.",
			run:   demoRegressionGate,
		},
	}
}

// demoInvoke runs one CLI command and returns its output. The command line is
// printed first, so the narration doubles as a transcript a reader can retype.
func demoInvoke(args ...string) (string, error) {
	var out bytes.Buffer
	if err := runCLI(args, &out); err != nil {
		return out.String(), fmt.Errorf("twinwright %s: %w", strings.Join(args, " "), err)
	}
	return out.String(), nil
}

func demoBuildWorld(ctx *demoContext) (string, error) {
	world := filepath.Join(ctx.examples, "company", "world.yaml")
	if _, err := demoInvoke("build-world", world, "--out", ctx.manifest); err != nil {
		return "", err
	}
	manifest, err := readManifest(ctx.manifest)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d operations compiled across the world's services", len(manifest.Operations)), nil
}

func demoRunIncident(ctx *demoContext) (string, error) {
	output, err := demoInvoke("run", "company-incident",
		"--agent", "scripted", "--seed", "42",
		"--chaos", filepath.Join(ctx.examples, "chaos", "company-incident.yaml"),
		"--manifest", ctx.manifest, "--db", ctx.db)
	if err != nil {
		return "", err
	}
	ctx.runID = runIDPattern.FindString(output)
	if ctx.runID == "" {
		return "", fmt.Errorf("the run produced no run ID: %s", demoExcerpt(output))
	}
	var result struct {
		Run struct {
			Status string `json:"status"`
			// Step is how many model turns the run consumed.
			Step int `json:"step"`
		} `json:"run"`
		Evaluation struct {
			Passed bool `json:"passed"`
		} `json:"evaluation"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return "", err
	}
	if result.Run.Status != "completed" {
		return "", fmt.Errorf("the incident did not complete, status %q", result.Run.Status)
	}
	if result.Run.Step == 0 {
		return "", fmt.Errorf("the run recorded no model turns: %s", demoExcerpt(output))
	}
	if !result.Evaluation.Passed {
		return "", fmt.Errorf("the incident completed but failed evaluation: %s", demoExcerpt(output))
	}
	return fmt.Sprintf("run %s completed in %d model turns and passed evaluation despite the injected faults",
		ctx.runID, result.Run.Step), nil
}

func demoInspect(ctx *demoContext) (string, error) {
	output, err := demoInvoke("inspect", ctx.runID, "--manifest", ctx.manifest, "--db", ctx.db)
	if err != nil {
		return "", err
	}
	var result struct {
		Summary struct {
			ToolCalls int `json:"tool_calls"`
		} `json:"summary"`
		Evaluation struct {
			Passed bool `json:"passed"`
		} `json:"evaluation"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return "", err
	}
	if result.Summary.ToolCalls == 0 {
		return "", fmt.Errorf("inspect reported no tool calls: %s", demoExcerpt(output))
	}
	if !result.Evaluation.Passed {
		return "", fmt.Errorf("inspect disagrees with the run's own verdict: %s", demoExcerpt(output))
	}
	return fmt.Sprintf("%d tool calls recorded, and the stored verdict still passes when re-read",
		result.Summary.ToolCalls), nil
}

func demoTrace(ctx *demoContext) (string, error) {
	output, err := demoInvoke("trace", ctx.runID, "--format", "otlp", "--db", ctx.db)
	if err != nil {
		return "", err
	}
	spans, err := countOTLPSpans(output)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("OTLP export with resourceSpans and %d spans", spans), nil
}

// countOTLPSpans reads the OTLP document and counts the spans in it. A document
// that names resourceSpans and holds no span is an empty export, not a trace.
func countOTLPSpans(output string) (int, error) {
	var doc struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []json.RawMessage `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal([]byte(output), &doc); err != nil {
		return 0, fmt.Errorf("the OTLP trace is not a JSON document (%v): %s", err, demoExcerpt(output))
	}
	if len(doc.ResourceSpans) == 0 {
		return 0, fmt.Errorf("the OTLP trace carries no resourceSpans: %s", demoExcerpt(output))
	}
	spans := 0
	for _, resource := range doc.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			spans += len(scope.Spans)
		}
	}
	if spans == 0 {
		return 0, fmt.Errorf("the OTLP trace has resourceSpans and no span in them: %s", demoExcerpt(output))
	}
	return spans, nil
}

func demoEvaluate(ctx *demoContext) (string, error) {
	output, err := demoInvoke("evaluate", ctx.runID,
		"--assertions", filepath.Join(ctx.examples, "assertions", "company-incident.yaml"),
		"--manifest", ctx.manifest, "--db", ctx.db)
	if err != nil {
		return "", err
	}
	// The evaluate command nests its report under "assertions", and the per-check
	// results are the only place the count of checks exists.
	var result struct {
		Assertions struct {
			Passed  bool `json:"passed"`
			Results []struct {
				ID     string `json:"id"`
				Passed bool   `json:"passed"`
				Detail string `json:"detail"`
			} `json:"results"`
		} `json:"assertions"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return "", err
	}
	total := len(result.Assertions.Results)
	if total == 0 {
		return "", fmt.Errorf("the assertion file evaluated nothing: %s", demoExcerpt(output))
	}
	failed := []string{}
	for _, check := range result.Assertions.Results {
		if !check.Passed {
			failed = append(failed, check.ID+" ("+check.Detail+")")
		}
	}
	if len(failed) > 0 || !result.Assertions.Passed {
		return "", fmt.Errorf("%d of %d assertions failed: %s", len(failed), total, strings.Join(failed, "; "))
	}
	return fmt.Sprintf("%d/%d assertions held against the world's final state and the ledger's order",
		total-len(failed), total), nil
}

func demoReplay(ctx *demoContext) (string, error) {
	output, err := demoInvoke("replay", ctx.runID, "--manifest", ctx.manifest, "--db", ctx.db)
	if err != nil {
		return "", err
	}
	var result struct {
		Verified bool `json:"verified"`
		// EventsCompared is how much of the ledger was actually re-executed. A
		// verified replay over zero events would be a vacuous pass.
		EventsCompared int `json:"events_compared"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return "", err
	}
	if !result.Verified {
		return "", fmt.Errorf("the ledger did not reproduce: %s", demoExcerpt(output))
	}
	if result.EventsCompared == 0 {
		return "", fmt.Errorf("replay verified nothing: no events were compared: %s", demoExcerpt(output))
	}
	return fmt.Sprintf("replay verified over %d recorded events", result.EventsCompared), nil
}

func demoForkAndCompare(ctx *demoContext) (string, error) {
	output, err := demoInvoke("checkpoints", ctx.runID, "--manifest", ctx.manifest, "--db", ctx.db)
	if err != nil {
		return "", err
	}
	var checkpoints struct {
		Checkpoints []struct {
			EventSeq int `json:"event_seq"`
		} `json:"checkpoints"`
	}
	if err := json.Unmarshal([]byte(output), &checkpoints); err != nil {
		return "", err
	}
	if len(checkpoints.Checkpoints) == 0 {
		return "", fmt.Errorf("the run exposed no committed boundary to fork from: %s", demoExcerpt(output))
	}
	at := checkpoints.Checkpoints[0].EventSeq
	forked, err := demoInvoke("fork", ctx.runID, "--at-event", fmt.Sprint(at),
		"--manifest", ctx.manifest, "--db", ctx.db)
	if err != nil {
		return "", err
	}
	childID := ""
	for _, id := range runIDPattern.FindAllString(forked, -1) {
		if id != ctx.runID {
			childID = id
			break
		}
	}
	if childID == "" {
		return "", fmt.Errorf("the fork produced no child run distinct from its parent: %s", demoExcerpt(forked))
	}
	if _, err := demoInvoke("compare", ctx.runID, childID, "--db", ctx.db); err != nil {
		return "", err
	}
	return fmt.Sprintf("forked at event %d into %s and compared the two worlds", at, childID), nil
}

func demoCrashAndTakeover(ctx *demoContext) (string, error) {
	output, err := demoInvoke("bench",
		"--suite-file", filepath.Join(ctx.examples, "bench", "flagship.yaml"),
		"--agent", "scripted", "--examples", ctx.examples,
		"--out", filepath.Join(ctx.dir, "flagship-bench.json"))
	if err != nil {
		return "", err
	}
	report, err := demoBenchReport(output)
	if err != nil {
		return "", err
	}
	return summarizeCrashRecovery(report, output)
}

// summarizeCrashRecovery requires the distributed cases to show a crash, a
// takeover under a higher fence, a refused stale commit and a replay that still
// verifies, and describes what the report says. output is the bench command's
// text, kept only to excerpt in a failure message.
func summarizeCrashRecovery(report demoBench, output string) (string, error) {
	if len(report.Cases) == 0 {
		return "", fmt.Errorf("the distributed suite ran no cases: %s", demoExcerpt(output))
	}
	crashes, takeovers, rejections, replays := 0, 0, 0, 0
	// fences are the distinct "crashed -> recovery" pairs the takeovers showed, in
	// the order they were first seen. The step prints these, not a constant.
	var fences []string
	for _, c := range report.Cases {
		if c.Distributed == nil {
			continue
		}
		d := c.Distributed
		if !c.Passed {
			return "", fmt.Errorf("case %s did not pass after its worker was crashed", c.ID)
		}
		if d.WorkerCrashed {
			crashes++
		}
		// A takeover must carry a strictly higher fence: equal fences would mean
		// two workers believing they own the same run, which is the bug fencing
		// exists to make impossible.
		if d.Takeover && d.RecoveryFence > d.CrashedFence {
			takeovers++
			if pair := fmt.Sprintf("%d -> %d", d.CrashedFence, d.RecoveryFence); !slices.Contains(fences, pair) {
				fences = append(fences, pair)
			}
		}
		rejections += d.FencingRejections
		if d.ReplayVerified {
			replays++
		}
	}
	if crashes == 0 || takeovers == 0 {
		return "", fmt.Errorf("no worker was crashed and recovered: %s", demoExcerpt(output))
	}
	if rejections == 0 {
		return "", fmt.Errorf("the crashed worker's late commit was never refused, so fencing was not exercised: %s", demoExcerpt(output))
	}
	if replays != len(report.Cases) {
		return "", fmt.Errorf("only %d of %d recovered runs still replay", replays, len(report.Cases))
	}
	return fmt.Sprintf("%d crashes, %d takeovers at fence %s, %d stale commit refused by fencing, replay verified on all %d",
		crashes, takeovers, strings.Join(fences, ", "), rejections, len(report.Cases)), nil
}

func demoFailOnPurpose(ctx *demoContext) (string, error) {
	// The failing run exits non-zero by design: the fixture assumes a write it
	// never made. What matters is that it produced a run to explain, so the run
	// ID is read out of the output rather than trusting the exit status.
	output, _ := demoInvoke("run", "company-incident",
		"--agent", "scripted", "--model", "fixture-unsafe-v1", "--seed", "42",
		"--chaos", filepath.Join(ctx.examples, "chaos", "company-refund-never-committed.yaml"),
		"--manifest", ctx.manifest, "--db", ctx.failedDB)
	ctx.failedID = runIDPattern.FindString(output)
	if ctx.failedID == "" {
		return "", fmt.Errorf("the failing run produced no run ID: %s", demoExcerpt(output))
	}
	inspected, err := demoInvoke("inspect", ctx.failedID, "--manifest", ctx.manifest, "--db", ctx.failedDB)
	if err != nil {
		return "", err
	}
	var result struct {
		Summary struct {
			// Faults is how many injected faults the run actually met.
			Faults int `json:"faults"`
		} `json:"summary"`
		Evaluation struct {
			Passed bool `json:"passed"`
			Checks []struct {
				Name   string `json:"name"`
				Passed bool   `json:"passed"`
			} `json:"checks"`
		} `json:"evaluation"`
	}
	if err := json.Unmarshal([]byte(inspected), &result); err != nil {
		return "", err
	}
	if result.Evaluation.Passed {
		return "", fmt.Errorf("the run meant to fail passed instead, so the fault was not injected: %s", demoExcerpt(inspected))
	}
	if result.Summary.Faults == 0 {
		return "", fmt.Errorf("the run failed without meeting any injected fault, so it failed for the wrong reason: %s", demoExcerpt(inspected))
	}
	failed := []string{}
	for _, check := range result.Evaluation.Checks {
		if !check.Passed {
			failed = append(failed, check.Name)
		}
	}
	// The specific check matters: this run must fail because the customer was
	// never made whole, not because some unrelated paperwork check tripped.
	unrefunded := false
	for _, name := range failed {
		if name == "exactly_one_refund" || name == "refund_amount_correct" {
			unrefunded = true
		}
	}
	if !unrefunded {
		return "", fmt.Errorf("the run failed on %s, none of which shows the refund missing: %s",
			strings.Join(failed, ", "), demoExcerpt(inspected))
	}
	return fmt.Sprintf("run %s met %d injected faults and failed on %s: the paperwork says refunded, the ledger says otherwise",
		ctx.failedID, result.Summary.Faults, strings.Join(failed, ", ")), nil
}

func demoCounterfactual(ctx *demoContext) (string, error) {
	output, err := demoInvoke("counterfactual", ctx.failedID,
		"--interventions", filepath.Join(ctx.examples, "counterfactual", "company-incident.yaml"),
		"--trials", "1", "--manifest", ctx.manifest, "--db", ctx.failedDB)
	if err != nil {
		return "", err
	}
	var result struct {
		Failure struct {
			Failed []string `json:"failed"`
		} `json:"failure"`
		Candidates []demoCandidate `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return "", err
	}
	if len(result.Candidates) == 0 {
		return "", fmt.Errorf("the counterfactual evaluated no interventions: %s", demoExcerpt(output))
	}
	if len(result.Failure.Failed) == 0 {
		return "", fmt.Errorf("the counterfactual found nothing to explain, so its parent did not fail: %s", demoExcerpt(output))
	}
	summary, err := summarizeCounterfactual(result.Candidates)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d interventions against %s. %s",
		len(result.Candidates), strings.Join(result.Failure.Failed, ", "), summary), nil
}

// demoCandidate is one intervention's result. Kind separates a change to the
// agent's own decision from a change to the world it acts on, which is the
// distinction the summary is built on.
type demoCandidate struct {
	Label         string `json:"label"`
	Kind          string `json:"kind"`
	CheckpointSeq int    `json:"checkpoint_seq"`
	Forks         int    `json:"forks"`
	Changed       int    `json:"changed"`
}

// summarizeCounterfactual describes the distribution rather than picking a
// winner. Naming the first intervention with any effect is actively misleading
// when most of them have one: on the flagship failure that names a customer read
// as the explanation for a lost refund write.
//
// What the distribution says is sharper than any single candidate. Changing the
// agent corrects the outcome at every checkpoint, including the last decision
// after the notification has already gone out, because a fixture that rebuilds
// its decision from the whole transcript still sees an unresolved refund that
// late. Changing the world corrects it only up to the write itself, and not once
// afterwards. The world's opportunity closes; the agent's does not.
//
// The interleaving check is what keeps that claim honest. Effects that stop and
// then resume are not a deadline, and the demo must not narrate the clean story
// over a messier one.
func summarizeCounterfactual(candidates []demoCandidate) (string, error) {
	agentTotal, agentChanged, lastAgentChanged := 0, 0, 0
	worldTotal, worldChanged := 0, 0
	lastWorldChanged, firstWorldUnchanged := 0, 0
	anyChanged := false
	for _, c := range candidates {
		if c.Changed > 0 {
			anyChanged = true
		}
		if c.Kind == "model" {
			agentTotal++
			if c.Changed > 0 {
				agentChanged++
				if c.CheckpointSeq > lastAgentChanged {
					lastAgentChanged = c.CheckpointSeq
				}
			}
			continue
		}
		worldTotal++
		if c.Changed > 0 {
			worldChanged++
			if c.CheckpointSeq > lastWorldChanged {
				lastWorldChanged = c.CheckpointSeq
			}
			continue
		}
		if firstWorldUnchanged == 0 || c.CheckpointSeq < firstWorldUnchanged {
			firstWorldUnchanged = c.CheckpointSeq
		}
	}
	if !anyChanged {
		return "", fmt.Errorf("no intervention changed the outcome, so the failure is unexplained")
	}
	if worldChanged > 0 && firstWorldUnchanged > 0 && lastWorldChanged > firstWorldUnchanged {
		return "", fmt.Errorf("world-side effects resume after stopping (last effective at event %d, first ineffective at event %d), so there is no single boundary to report",
			lastWorldChanged, firstWorldUnchanged)
	}
	summary := fmt.Sprintf("%d/%d agent-side changes corrected it, the latest at event %d",
		agentChanged, agentTotal, lastAgentChanged)
	if worldChanged == worldTotal {
		return summary + fmt.Sprintf("; every one of the %d world-side changes corrected it too", worldTotal), nil
	}
	return summary + fmt.Sprintf("; only %d/%d world-side changes did, none after event %d, so the world's opportunity closes and the agent's does not",
		worldChanged, worldTotal, lastWorldChanged), nil
}

func demoSecurity(ctx *demoContext) (string, error) {
	blocked, err := demoInvoke("run", securityScenario,
		"--agent", "scripted", "--seed", "42",
		"--auth", filepath.Join(ctx.examples, "security", "support-policy.yaml"),
		"--manifest", ctx.manifest, "--db", filepath.Join(ctx.dir, "security.db"))
	if err != nil {
		return "", err
	}
	attempted, refused, err := checkInjectionRefused(blocked)
	if err != nil {
		return "", err
	}
	// The over-privileged principal is the negative control: it is allowed to
	// reach another customer's data, so the run must fail. A pass here would mean
	// the authorization checks stopped looking.
	over, _ := demoInvoke("run", securityScenario,
		"--agent", "scripted", "--seed", "42",
		"--auth", filepath.Join(ctx.examples, "security", "overprivileged-policy.yaml"),
		"--manifest", ctx.manifest, "--db", filepath.Join(ctx.dir, "security-over.db"))
	overID := runIDPattern.FindString(over)
	if overID == "" {
		return "", fmt.Errorf("the over-privileged run produced no run ID: %s", demoExcerpt(over))
	}
	leaked, err := checkOverPrivilegedCaught(over)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("the injected instruction was attempted at %d call(s) and refused at %d under the support policy, and none got through; the over-privileged principal let %d through and was caught",
		attempted, refused, leaked), nil
}

// demoFinding is one entry of a run's security analysis: whether the ledger shows
// that kind of call, and the events that show it.
type demoFinding struct {
	Detected bool     `json:"detected"`
	EventIDs []string `json:"event_ids"`
}

// demoSecurityRun is the part of a security scenario's run output the step reads.
// Security is a pointer so a document without the analysis is told apart from one
// whose analysis found nothing.
type demoSecurityRun struct {
	Evaluation struct {
		Passed bool `json:"passed"`
	} `json:"evaluation"`
	Security *struct {
		Attempted  demoFinding `json:"attempted_violation"`
		Blocked    demoFinding `json:"blocked_violation"`
		Successful demoFinding `json:"successful_violation"`
	} `json:"security"`
}

// parseSecurityRun decodes a security scenario's output and insists on the
// analysis. The scenario's name is in the output of every run, so nothing short
// of the analysis itself says whether the injected instruction was refused.
func parseSecurityRun(output string) (demoSecurityRun, error) {
	var run demoSecurityRun
	if err := json.Unmarshal([]byte(output), &run); err != nil {
		return run, fmt.Errorf("the run printed no JSON document (%v): %s", err, demoExcerpt(output))
	}
	if run.Security == nil {
		return run, fmt.Errorf("the run printed no security analysis: %s", demoExcerpt(output))
	}
	return run, nil
}

// checkInjectionRefused requires the support policy's run to show the injected
// instruction attempted, refused, and never carried out, and returns how many
// calls were attempted and how many were refused. Without a security analysis in
// the output there is no finding, and the step fails.
func checkInjectionRefused(output string) (attempted, refused int, err error) {
	run, err := parseSecurityRun(output)
	if err != nil {
		return 0, 0, err
	}
	security := run.Security
	if !security.Attempted.Detected || len(security.Attempted.EventIDs) == 0 {
		return 0, 0, fmt.Errorf("the security analysis recorded no attempt to follow the injected instruction, so the run shows nothing was refused: %s", demoExcerpt(output))
	}
	if !security.Blocked.Detected || len(security.Blocked.EventIDs) == 0 {
		return 0, 0, fmt.Errorf("the security analysis shows the injected instruction was never refused: %s", demoExcerpt(output))
	}
	if security.Successful.Detected {
		return 0, 0, fmt.Errorf("the injected instruction got through under the support policy: %s", demoExcerpt(output))
	}
	if !run.Evaluation.Passed {
		return 0, 0, fmt.Errorf("the support principal was expected to handle the ticket safely: %s", demoExcerpt(output))
	}
	return len(security.Attempted.EventIDs), len(security.Blocked.EventIDs), nil
}

// checkOverPrivilegedCaught requires the negative control to be readable and to
// show what it exists to show: the over-privileged principal's run fails its
// evaluation because the ledger records a violation that succeeded. An output
// that cannot be read is a failure, not a pass. It returns how many calls got
// through.
func checkOverPrivilegedCaught(output string) (leaked int, err error) {
	run, err := parseSecurityRun(output)
	if err != nil {
		return 0, err
	}
	if run.Evaluation.Passed {
		return 0, fmt.Errorf("the over-privileged principal was not caught: %s", demoExcerpt(output))
	}
	if !run.Security.Successful.Detected || len(run.Security.Successful.EventIDs) == 0 {
		return 0, fmt.Errorf("the over-privileged run failed, but the security analysis records no violation that succeeded, so it did not fail for the reason the control exists to show: %s", demoExcerpt(output))
	}
	return len(run.Security.Successful.EventIDs), nil
}

// demoRegressionGate benches the deliberately regressed fixture and requires
// the gate to block it. Exit code 1 means regressions, 2 means the gate could
// not decide, and only the first is evidence: a gate that errors out has not
// shown it can tell a regression from a healthy run.
func demoRegressionGate(ctx *demoContext) (string, error) {
	candidate := filepath.Join(ctx.dir, "regressed-bench.json")
	if _, err := demoInvoke("bench", "--suite", "standard", "--agent", "scripted",
		"--model", agent.RegressedModel, "--examples", ctx.examples,
		"--db-dir", filepath.Join(ctx.dir, "regressed-dbs"), "--out", candidate); err != nil {
		return "", err
	}
	output, err := demoInvoke("gate",
		"--baseline", filepath.Join(ctx.examples, "bench", "baselines", "standard-scripted.json"),
		"--candidate", candidate)
	if exitCode(err) != exitRegression {
		return "", fmt.Errorf("the gate was expected to exit %d on the regressed fixture, got %d (%v): %s",
			exitRegression, exitCode(err), err, demoExcerpt(output))
	}
	var result gate.Result
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return "", fmt.Errorf("the gate printed no verdict: %w", err)
	}
	regressed, err := readBenchReport(candidate)
	if err != nil {
		return "", err
	}
	if result.Passed {
		return "", fmt.Errorf("the gate passed the regressed fixture: %s", demoExcerpt(output))
	}
	if err := missingWriteRegression(result, regressed); err != nil {
		return "", fmt.Errorf("%w: %s", err, demoExcerpt(output))
	}
	cases := map[string]bool{}
	for _, r := range result.Regressions {
		cases[r.Case] = true
	}
	return fmt.Sprintf("exit %d: %d regressed cases in %d compared, including billing-duplicate-charge",
		exitRegression, len(cases), result.Compared), nil
}

// missingWriteRegression checks the one regression the regressed fixture is
// built to cause: billing-duplicate-charge, which needs a refund, is judged
// newly failing because the candidate failed it without writing. Any other
// regression would not do. Two other cases error under the fixture (no crash is
// injected, and a counterfactual's parent has no call to remove), and each of
// those is a regression on its own, so a check that accepted any regression, or
// any regression naming the case, would pass even if the failed-status path were
// broken. The demo step, the CI step and TestRegressedBenchFailsTheGate state
// the same requirement; this is the Go form of it.
func missingWriteRegression(result gate.Result, candidate bench.Report) error {
	const id = "billing-duplicate-charge"
	var finding *gate.Finding
	for i := range result.Regressions {
		if r := result.Regressions[i]; r.Case == id && r.Kind == gate.KindNewlyFailing {
			finding = &result.Regressions[i]
			break
		}
	}
	if finding == nil {
		return fmt.Errorf("the gate has no %s regression for %s", gate.KindNewlyFailing, id)
	}
	if !strings.Contains(finding.Detail, "candidate failed") {
		return fmt.Errorf("the %s regression for %s says %q, not that the candidate failed", gate.KindNewlyFailing, id, finding.Detail)
	}
	for _, c := range candidate.Cases {
		if c.ID != id {
			continue
		}
		if c.Status != "failed" || c.Mutations != 0 {
			return fmt.Errorf("the candidate's %s has status %q and %d mutations, want failed with none", id, c.Status, c.Mutations)
		}
		return nil
	}
	return fmt.Errorf("the candidate report has no case %s", id)
}

// demoBench is the part of a bench report the crash step reads.
type demoBench struct {
	Cases []demoBenchCase `json:"cases"`
}

type demoBenchCase struct {
	ID          string           `json:"id"`
	Passed      bool             `json:"passed"`
	Distributed *demoDistributed `json:"distributed"`
}

type demoDistributed struct {
	WorkerCrashed     bool  `json:"worker_crashed"`
	CrashedFence      int64 `json:"crashed_fence"`
	Takeover          bool  `json:"takeover"`
	RecoveryFence     int64 `json:"recovery_fence"`
	FencingRejections int   `json:"fencing_rejections"`
	ReplayVerified    bool  `json:"replay_verified"`
}

// demoBenchReport parses the bench report the CLI prints after its text summary.
func demoBenchReport(output string) (demoBench, error) {
	var report demoBench
	start := strings.Index(output, `{"suite"`)
	if start < 0 {
		return report, fmt.Errorf("the bench command printed no JSON report: %s", demoExcerpt(output))
	}
	if err := json.Unmarshal([]byte(output[start:]), &report); err != nil {
		return report, err
	}
	return report, nil
}

// demoExcerpt keeps a failure message readable when a step's output is a large
// JSON document. The ellipsis is three ASCII dots: the demo's output goes to
// terminals and CI logs of every encoding.
func demoExcerpt(output string) string {
	output = strings.TrimSpace(output)
	if len(output) > 400 {
		return output[:400] + "..."
	}
	return output
}
