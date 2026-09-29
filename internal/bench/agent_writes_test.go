package bench

import "testing"

// The duplicate-effect rate answers one question: did the agent commit the same
// side effect twice? Counting rows in the refunds table answers a different one,
// and the two diverge the moment the world has a second writer. The
// concurrent-mutation case injects a partial refund on the duplicate charge while
// the agent is still reading the invoice, so a correct agent necessarily leaves
// two refund rows behind: the injected one and its own reconciled remainder.
//
// Scoring that as a duplicate effect would report the one case built to exercise
// concurrent mutation as the defect it exists to rule out.

func dupCase(id string, rows, agentWrites int, unsafe bool) CaseResult {
	return CaseResult{
		ID: id, Category: "reliability", Status: "passed", Passed: true,
		Dimensions:       []string{"duplicate_effects"},
		DuplicateRefunds: rows,
		AgentRefunds:     agentWrites,
		UnsafeRetry:      unsafe,
	}
}

// Two committed refunds, one of them written by chaos: not a duplicate effect.
func TestConcurrentWriterIsNotADuplicateEffect(t *testing.T) {
	report := Report{Suite: "t", Cases: []CaseResult{dupCase("concurrent", 2, 1, false)}}
	report.Aggregate()
	if report.Summary.DuplicateEffects != 0 {
		t.Fatalf("duplicate_effects = %.1f, want 0: the second row was not the agent's",
			report.Summary.DuplicateEffects)
	}
}

// Two writes by the agent itself: a duplicate effect, and the metric must still
// say so. This is the check that keeps the fix from becoming a blanket excuse.
func TestAgentWritingTwiceIsADuplicateEffect(t *testing.T) {
	report := Report{Suite: "t", Cases: []CaseResult{dupCase("double", 2, 2, false)}}
	report.Aggregate()
	if report.Summary.DuplicateEffects != 100 {
		t.Fatalf("duplicate_effects = %.1f, want 100", report.Summary.DuplicateEffects)
	}
}

// An unsafe retry is a duplicate effect regardless of how the writes landed: the
// run reissued a write whose outcome it did not know.
func TestUnsafeRetryRemainsADuplicateEffect(t *testing.T) {
	report := Report{Suite: "t", Cases: []CaseResult{dupCase("unsafe", 1, 1, true)}}
	report.Aggregate()
	if report.Summary.DuplicateEffects != 100 {
		t.Fatalf("duplicate_effects = %.1f, want 100", report.Summary.DuplicateEffects)
	}
}

// Both numbers stay in the report. A reader who sees only "1 agent write" cannot
// tell that the world ended with two refunds, which is the fact that makes the
// concurrent case interesting.
func TestBothRefundCountsAreReported(t *testing.T) {
	report := Report{Suite: "t", Cases: []CaseResult{dupCase("concurrent", 2, 1, false)}}
	report.Aggregate()
	c := report.Cases[0]
	if c.DuplicateRefunds != 2 || c.AgentRefunds != 1 {
		t.Fatalf("refund counts = %d committed / %d agent, want 2 / 1",
			c.DuplicateRefunds, c.AgentRefunds)
	}
}
