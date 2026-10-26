package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"twinwright/internal/agent"
	"twinwright/internal/assertion"
	"twinwright/internal/compiler"
	"twinwright/internal/counterfactual"
	"twinwright/internal/crashmatrix"
	"twinwright/internal/crashpoint"
	"twinwright/internal/store"
)

// Distributed and counterfactual execution modes.
//
// A local case runs the agent in process, which is what keeps the deterministic
// examples simple. These two modes exist because the runtime's two hardest
// features were invisible to its own benchmark: nothing crashed a worker, and
// nothing explained a failure.
//
// Both stay deterministic. The distributed mode drives a controlled clock rather
// than sleeping out a lease TTL, and the crash is injected at a named durable
// boundary rather than at whatever moment a timer happens to fire.

const benchLeaseTTL = time.Minute

// benchClockStart is the instant the controlled clock reads when a distributed
// case begins.
var benchClockStart = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// runDistributed executes one case through the crash matrix's harness: a first
// worker dies at the case's crash point, a second takes the run over, and the
// matrix's verifier checks the recovery. The bench used to carry its own copy of
// that sequence, which could only crash at a model turn; routing it through the
// harness is what lets a case die after a tool commit, and keeps the bench and
// the matrix from drifting apart on what "recovered" means.
//
// There is no uncrashed reference run here, so the verifier is given a nil
// Reference and skips the checks that compare against one. The case's own judge
// still decides whether the recovered world is right.
func runDistributed(ctx context.Context, s *store.Store, c Case, manifest compiler.Manifest, provider agent.Provider, run store.Run) (store.Run, DistributedResult, error) {
	var measured DistributedResult
	point := crashpoint.AfterModelRequest
	if c.CrashPoint != "" {
		parsed, err := crashpoint.ParsePoint(c.CrashPoint)
		if err != nil {
			return run, measured, err
		}
		point = parsed
	}
	crashCase := crashmatrix.Case{Point: point, Occurrence: c.CrashAt, Operation: c.CrashOperation}
	measured.CrashPoint = string(point)
	measured.CrashOperation = c.CrashOperation
	measured.Occurrence = c.CrashAt

	target := crashmatrix.Target{
		Manifest:          manifest,
		Provider:          provider,
		Steps:             c.Steps,
		MutatingOperation: zombieOperation(c.World),
		ZombieArguments:   zombieArguments(c.World),
	}
	clock := crashmatrix.NewClock(benchClockStart)
	if err := s.Enqueue(ctx, run.ID, c.Steps, clock.Now()); err != nil {
		return run, measured, fmt.Errorf("enqueue: %w", err)
	}

	crash, err := crashmatrix.Crash(ctx, s, target, run.ID, crashCase, clock, benchLeaseTTL)
	measured.CrashedFence = crash.Doomed.Token
	measured.Deliveries = 1
	measured.WorkerCrashed = crash.Crashed
	if err != nil {
		return run, measured, err
	}
	// A crash point the run never reached measures nothing, and reporting it as
	// a successful recovery would be a false positive. It is an error.
	if !crash.Crashed {
		finished, readErr := s.Run(ctx, run.ID)
		if readErr != nil {
			return run, measured, readErr
		}
		return finished, measured, fmt.Errorf("the run stopped with status %q before %s #%d was reached, so no crash was injected", finished.Status, point, c.CrashAt)
	}

	// The dead worker's lease lapses and a second worker takes the run over.
	clock.Advance(2 * benchLeaseTTL)
	outcome, err := crashmatrix.Recover(ctx, s, target, clock, benchLeaseTTL)
	if err != nil {
		// Recover's own errors already say which step failed.
		return run, measured, err
	}

	verdict := crashmatrix.Verify(ctx, s, target, nil, run.ID, crash, outcome)
	measured.Takeover = verdict.Takeover
	measured.RecoveryFence = verdict.RecoveryFence
	measured.Deliveries = verdict.Deliveries
	measured.ReplayVerified = verdict.ReplayVerified
	// One rejection per case is what this field has always counted: the stale
	// worker's attempt to commit a new call. The verifier also tries a call the
	// run already committed, but folding that in would silently double every
	// recorded count, so it is reported through the violations instead.
	if verdict.StaleNewCallRejected {
		measured.FencingRejections = 1
	}
	measured.Violations = verdict.Violations

	recovered, err := s.Run(ctx, run.ID)
	if err != nil {
		return run, measured, err
	}
	if len(verdict.Violations) > 0 {
		return recovered, measured, fmt.Errorf("recovery after a crash at %s #%d broke %d invariant(s): %s",
			point, c.CrashAt, len(verdict.Violations), strings.Join(verdict.Violations, "; "))
	}
	return recovered, measured, nil
}

// zombieOperation is the mutation the stale worker attempts. It is a write, so
// that a missing fence check would show up as a committed effect rather than as
// a harmless read.
func zombieOperation(world string) string {
	if world == "company" {
		return "billing.createRefund"
	}
	return "createRefund"
}

func zombieArguments(string) map[string]any {
	return map[string]any{"charge_id": "CH-1002", "amount_cents": 500, "reason": "stale worker retry"}
}

// runCounterfactual explains a case's failure by intervention analysis.
func runCounterfactual(ctx context.Context, s *store.Store, c Case, manifest compiler.Manifest, options Options, run store.Run) (CounterfactualResult, error) {
	var measured CounterfactualResult
	path, err := Resolve(options.ExamplesRoot, c.Interventions)
	if err != nil {
		return measured, err
	}
	raw, err := os.ReadFile(filepath.Join(options.ExamplesRoot, filepath.FromSlash(path)))
	if err != nil {
		return measured, err
	}
	set, err := counterfactual.Parse(raw, manifest)
	if err != nil {
		return measured, err
	}
	judge := counterfactual.ScenarioJudge()
	if c.Assertions != "" {
		assertionPath, err := Resolve(options.ExamplesRoot, c.Assertions)
		if err != nil {
			return measured, err
		}
		rawAssertions, err := os.ReadFile(filepath.Join(options.ExamplesRoot, filepath.FromSlash(assertionPath)))
		if err != nil {
			return measured, err
		}
		parsed, err := assertion.Parse(rawAssertions, manifest, assertion.Builtins())
		if err != nil {
			return measured, err
		}
		judge = counterfactual.AssertionJudge(parsed)
	}

	// Prepare refuses a parent that passes its judge, which is the invariant
	// that keeps this case honest: an analysis of a passing run explains nothing.
	analysis, err := counterfactual.Prepare(ctx, s, run.ID, manifest, set, judge, counterfactual.Options{
		Trials: 1,
		Steps:  c.Steps,
		ProviderFor: func(provider, model, scenario string) (agent.Provider, error) {
			return options.ProviderFor(provider, model, scenario, options.BaseURL)
		},
	})
	if err != nil {
		return measured, err
	}
	report, err := analysis.Run(ctx, s, s)
	if err != nil {
		return measured, err
	}
	measured.ParentPassed = false
	measured.ParentFailed = report.Failure.Failed
	measured.Candidates = len(report.Candidates)
	for _, candidate := range report.Candidates {
		if candidate.Changed > 0 {
			measured.Explained = true
			measured.Explanation = candidate.Summary
			break
		}
	}
	return measured, nil
}
