package bench

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Report is the machine-readable bench output.
type Report struct {
	Suite      string         `json:"suite"`
	Digest     string         `json:"digest"`
	Agent      string         `json:"agent"`
	Model      string         `json:"model"`
	Scenarios  int            `json:"scenarios"`
	Summary    Summary        `json:"summary"`
	Cases      []CaseResult   `json:"cases"`
	Categories map[string]int `json:"categories"`
}

// Summary holds aggregated rates. DuplicateEffects is a failure rate; the other
// rates are success rates. The three distributed fields are counts rather than
// rates, because "how many times a stale worker was refused" is a fact about the
// run and not a proportion of anything.
type Summary struct {
	TaskSuccess         float64 `json:"task_success"`
	SafetyCompliance    float64 `json:"safety_compliance"`
	AuthorizationSafety float64 `json:"authorization_safety"`
	RecoverySuccess     float64 `json:"recovery_success"`
	DuplicateEffects    float64 `json:"duplicate_effects"`
	ExplanationRate     float64 `json:"explanation_rate"`
	MedianToolCalls     float64 `json:"median_tool_calls"`
	MedianLatencyMS     float64 `json:"median_latency_ms"`
	WorkerTakeovers     int     `json:"worker_takeovers"`
	DuplicateDeliveries int     `json:"duplicate_deliveries"`
	FencingRejections   int     `json:"fencing_rejections"`
}

// DistributedResult records what a distributed case observed. Every field is
// read back from the ledger, the queue or the store after the fact, so a case
// cannot report a takeover that the runtime did not perform.
type DistributedResult struct {
	// WorkerCrashed is true when the first worker died mid-run, leaving an
	// unanswered model request behind.
	WorkerCrashed bool `json:"worker_crashed"`
	// CrashedFence is the dead worker's fencing token.
	CrashedFence int64 `json:"crashed_fence"`
	// Takeover is true when the runtime reported the second claim as displacing
	// a previous holder rather than as a first claim.
	Takeover bool `json:"takeover"`
	// RecoveryFence must strictly exceed CrashedFence.
	RecoveryFence int64 `json:"recovery_fence"`
	// Deliveries is how many times the run was handed to a worker. Two or more
	// is at-least-once delivery doing exactly what it says.
	Deliveries int `json:"deliveries"`
	// FencingRejections counts commits refused because the presenting worker no
	// longer owned the run.
	FencingRejections int `json:"fencing_rejections"`
	// ReplayVerified is whether the recovered ledger still reproduces. A run
	// that survives a crash but can no longer be audited has lost the property
	// the project exists to provide.
	ReplayVerified bool `json:"replay_verified"`
}

// CounterfactualResult records what an intervention analysis found. The parent
// must fail: a counterfactual explains a failure, so a passing parent means the
// case measured nothing.
type CounterfactualResult struct {
	ParentPassed bool     `json:"parent_passed"`
	ParentFailed []string `json:"parent_failed,omitempty"`
	// Candidates is how many interventions were evaluated.
	Candidates int `json:"candidates"`
	// Explained is true when at least one intervention flipped the outcome.
	Explained bool `json:"explained"`
	// Explanation names the intervention that flipped it. With several, the
	// first in the analysis's own deterministic order is named.
	Explanation string `json:"explanation,omitempty"`
}

// CaseResult is one case outcome.
type CaseResult struct {
	ID               string   `json:"id"`
	Category         string   `json:"category"`
	Status           string   `json:"status"`
	RunID            string   `json:"run_id,omitempty"`
	Model            string   `json:"model,omitempty"`
	FailedChecks     []string `json:"failed_checks,omitempty"`
	ToolCalls        int      `json:"tool_calls"`
	ModelTurns       int      `json:"model_turns"`
	WallMS           int64    `json:"wall_ms"`
	DuplicateRefunds int      `json:"duplicate_refunds,omitempty"`
	UnsafeRetry      bool     `json:"unsafe_retry,omitempty"`
	Error            string   `json:"error,omitempty"`
	Dimensions       []string `json:"dimensions"`
	Passed           bool     `json:"passed"`

	// Distributed is set for mode: distributed cases only. It is a pointer so a
	// local case omits it entirely: a zeroed block would show replay_verified
	// false on a case that never replayed anything, which reads as a failure.
	Distributed *DistributedResult `json:"distributed,omitempty"`
	// Counterfactual is set for mode: counterfactual cases only, for the same
	// reason.
	Counterfactual *CounterfactualResult `json:"counterfactual,omitempty"`
}

// Aggregate fills Summary and category counts from Cases.
func (r *Report) Aggregate() {
	r.Scenarios = len(r.Cases)
	r.Categories = map[string]int{}
	var toolCalls []float64
	var latencies []float64
	counts := map[string]struct{ pass, total int }{}
	dupBad, dupTotal := 0, 0
	explained, explainable := 0, 0
	takeovers, deliveries, rejections := 0, 0, 0
	for _, c := range r.Cases {
		r.Categories[c.Category]++
		toolCalls = append(toolCalls, float64(c.ToolCalls))
		latencies = append(latencies, float64(c.WallMS))
		if c.Distributed != nil {
			if c.Distributed.Takeover {
				takeovers++
			}
			if c.Distributed.Deliveries > 1 {
				deliveries += c.Distributed.Deliveries - 1
			}
			rejections += c.Distributed.FencingRejections
		}
		for _, d := range c.Dimensions {
			switch d {
			case "duplicate_effects":
				dupTotal++
				if c.UnsafeRetry || c.DuplicateRefunds > 1 {
					dupBad++
				}
				continue
			case "explanation":
				explainable++
				if c.Counterfactual != nil && c.Counterfactual.Explained {
					explained++
				}
				continue
			}
			stat := counts[d]
			stat.total++
			if c.Passed && c.Status == "passed" {
				stat.pass++
			}
			counts[d] = stat
		}
	}
	r.Summary = Summary{
		TaskSuccess:         rate(counts["task"]),
		SafetyCompliance:    rate(counts["safety"]),
		AuthorizationSafety: rate(counts["authorization"]),
		RecoverySuccess:     rate(counts["recovery"]),
		DuplicateEffects:    pct(dupBad, dupTotal),
		ExplanationRate:     pct(explained, explainable),
		MedianToolCalls:     median(toolCalls),
		MedianLatencyMS:     median(latencies),
		WorkerTakeovers:     takeovers,
		DuplicateDeliveries: deliveries,
		FencingRejections:   rejections,
	}
}

func rate(s struct{ pass, total int }) float64 {
	return pct(s.pass, s.total)
}

func pct(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return math.Round(1000*float64(n)/float64(d)) / 10
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return math.Round((sorted[mid-1]+sorted[mid])/2*10) / 10
}

// FormatText renders the roadmap-style summary. The distributed lines are
// printed only when a case measured them, so a suite without distributed cases
// does not display three zeroes that look like failures.
func (r Report) FormatText() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Suite: %s (%d scenarios)\n", r.Suite, r.Scenarios)
	fmt.Fprintf(&b, "Agent: %s  Model: %s\n\n", r.Agent, r.Model)
	fmt.Fprintf(&b, "Task Success             %5.1f%%\n", r.Summary.TaskSuccess)
	fmt.Fprintf(&b, "Safety Compliance        %5.1f%%\n", r.Summary.SafetyCompliance)
	fmt.Fprintf(&b, "Authorization Safety     %5.1f%%\n", r.Summary.AuthorizationSafety)
	fmt.Fprintf(&b, "Recovery Success         %5.1f%%\n", r.Summary.RecoverySuccess)
	fmt.Fprintf(&b, "Duplicate Effects (bad)  %5.1f%%\n", r.Summary.DuplicateEffects)
	if r.Categories["counterfactual"] > 0 {
		fmt.Fprintf(&b, "Failures Explained       %5.1f%%\n", r.Summary.ExplanationRate)
	}
	if r.Categories["distributed"] > 0 {
		fmt.Fprintf(&b, "Worker Takeovers         %5d\n", r.Summary.WorkerTakeovers)
		fmt.Fprintf(&b, "Duplicate Deliveries     %5d\n", r.Summary.DuplicateDeliveries)
		fmt.Fprintf(&b, "Fencing Rejections       %5d\n", r.Summary.FencingRejections)
	}
	fmt.Fprintf(&b, "Median Tool Calls        %5.1f\n", r.Summary.MedianToolCalls)
	fmt.Fprintf(&b, "Median Latency           %5.1f ms\n", r.Summary.MedianLatencyMS)
	return b.String()
}

// CompareReports returns measurement deltas. It does not declare a winner.
func CompareReports(a, b Report) map[string]any {
	delta := func(name string, x, y float64) map[string]any {
		return map[string]any{"metric": name, "a": x, "b": y, "delta_b_minus_a": math.Round((y-x)*10) / 10}
	}
	return map[string]any{
		"kind": "bench_report_compare",
		"a":    map[string]any{"suite": a.Suite, "agent": a.Agent, "model": a.Model, "scenarios": a.Scenarios},
		"b":    map[string]any{"suite": b.Suite, "agent": b.Agent, "model": b.Model, "scenarios": b.Scenarios},
		"deltas": []map[string]any{
			delta("task_success", a.Summary.TaskSuccess, b.Summary.TaskSuccess),
			delta("safety_compliance", a.Summary.SafetyCompliance, b.Summary.SafetyCompliance),
			delta("authorization_safety", a.Summary.AuthorizationSafety, b.Summary.AuthorizationSafety),
			delta("recovery_success", a.Summary.RecoverySuccess, b.Summary.RecoverySuccess),
			delta("duplicate_effects", a.Summary.DuplicateEffects, b.Summary.DuplicateEffects),
			delta("explanation_rate", a.Summary.ExplanationRate, b.Summary.ExplanationRate),
			delta("worker_takeovers", float64(a.Summary.WorkerTakeovers), float64(b.Summary.WorkerTakeovers)),
			delta("duplicate_deliveries", float64(a.Summary.DuplicateDeliveries), float64(b.Summary.DuplicateDeliveries)),
			delta("fencing_rejections", float64(a.Summary.FencingRejections), float64(b.Summary.FencingRejections)),
			delta("median_tool_calls", a.Summary.MedianToolCalls, b.Summary.MedianToolCalls),
			delta("median_latency_ms", a.Summary.MedianLatencyMS, b.Summary.MedianLatencyMS),
		},
	}
}

// DecodeReport parses a JSON bench report.
func DecodeReport(data []byte) (Report, error) {
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return Report{}, err
	}
	if report.Suite == "" || report.Cases == nil {
		return Report{}, fmt.Errorf("not a bench report")
	}
	return report, nil
}
