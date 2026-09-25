package counterfactual

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"twinwright/internal/agent"
	"twinwright/internal/checkpoint"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/fork"
	"twinwright/internal/store"
)

const method = "intervention analysis: each fork changes one controlled variable at one checkpoint and is executed and judged independently; counts are counterfactual sensitivity over the forks run, not formal causal inference"

type Report struct {
	RunID         string      `json:"run_id"`
	Method        string      `json:"method"`
	Failure       Failure     `json:"failure"`
	Interventions string      `json:"interventions_digest"`
	Trials        int         `json:"trials"`
	Candidates    []Candidate `json:"candidates"`
}

// Failure is the parent's judged outcome that the forks try to change.
type Failure struct {
	Judge  string   `json:"judge"`
	Digest string   `json:"digest,omitempty"`
	Failed []string `json:"failed"`
}

// Candidate is one intervention applied at one parent event.
type Candidate struct {
	EventSeq      int       `json:"event_seq"`
	EventType     string    `json:"event_type"`
	Label         string    `json:"label"`
	CallID        string    `json:"call_id"`
	OperationID   string    `json:"operation_id"`
	Intervention  string    `json:"intervention"`
	Kind          string    `json:"kind"`
	CheckpointSeq int       `json:"checkpoint_seq"`
	Forks         int       `json:"forks"`
	Changed       int       `json:"changed"`
	Errored       int       `json:"errored"`
	Incomplete    int       `json:"incomplete"`
	Summary       string    `json:"summary"`
	Evidence      []Outcome `json:"evidence"`
}

// Outcome is one fork's result. Passed is set only for completed forks.
type Outcome struct {
	RunID  string   `json:"run_id"`
	Status string   `json:"status"`
	Passed *bool    `json:"passed,omitempty"`
	Failed []string `json:"failed,omitempty"`
	Error  string   `json:"error,omitempty"`
}

type Options struct {
	Trials      int
	Steps       int
	ProviderFor func(provider, model, scenario string) (agent.Provider, error)
}

type call struct {
	id, operation               string
	request, response, decision int
}

type plan struct {
	candidate  Candidate
	checkpoint checkpoint.Checkpoint
	options    fork.Options
	provider   agent.Provider
}

// Analysis is a validated set of forks to run against one parent.
type Analysis struct {
	header   Report
	plans    []plan
	manifest compiler.Manifest
	judge    Judge
	options  Options
}

// Analyze prepares and runs an analysis in one call.
func Analyze(ctx context.Context, source, destination *store.Store, runID string, manifest compiler.Manifest, set Set, judge Judge, options Options) (Report, error) {
	analysis, err := Prepare(ctx, source, runID, manifest, set, judge, options)
	if err != nil {
		return Report{}, err
	}
	return analysis.Run(ctx, source, destination)
}

// Prepare checks that the run is a completed, failing root run and validates
// every fork it will create. It only reads.
func Prepare(ctx context.Context, source *store.Store, runID string, manifest compiler.Manifest, set Set, judge Judge, options Options) (*Analysis, error) {
	if len(set.interventions) == 0 || set.manifest.Digest != manifest.Digest {
		return nil, fmt.Errorf("intervention set was not parsed for this manifest")
	}
	if judge.check == nil {
		return nil, fmt.Errorf("judge is required")
	}
	if options.Trials < 1 || options.Trials > 100 || options.Steps < 1 || options.ProviderFor == nil {
		return nil, fmt.Errorf("counterfactual analysis requires 1 to 100 trials, positive steps, and a provider source")
	}
	parent, err := source.Run(ctx, runID)
	if err != nil {
		return nil, err
	}
	if parent.Status != "completed" {
		return nil, fmt.Errorf("run %s is %s; counterfactual analysis requires a completed run", runID, parent.Status)
	}
	points, err := checkpoint.List(ctx, source, runID, manifest)
	if err != nil {
		return nil, err
	}
	events, err := source.Events(ctx, runID)
	if err != nil {
		return nil, err
	}
	passed, failed, err := judge.check(ctx, source, parent)
	if err != nil {
		return nil, err
	}
	if passed {
		return nil, fmt.Errorf("run %s passes its %s; counterfactual analysis explains failures", runID, judge.Name)
	}
	calls, err := discoverCalls(events)
	if err != nil {
		return nil, err
	}
	plans, err := planForks(ctx, source, parent, manifest, set, calls, points, options)
	if err != nil {
		return nil, err
	}
	header := Report{RunID: runID, Method: method, Failure: Failure{Judge: judge.Name, Digest: judge.Digest, Failed: failed},
		Interventions: set.Digest(), Trials: options.Trials}
	return &Analysis{header: header, plans: plans, manifest: manifest, judge: judge, options: options}, nil
}

// Run forks the parent once per candidate and trial, executes each child, and
// ranks candidates by how often the judged outcome changed. The source must be
// a read-only handle to the destination's database file.
func (a *Analysis) Run(ctx context.Context, source, destination *store.Store) (Report, error) {
	report := a.header
	report.Candidates = []Candidate{}
	for _, p := range a.plans {
		c := p.candidate
		for trial := 0; trial < a.options.Trials; trial++ {
			outcome, err := runFork(ctx, source, destination, a.manifest, p, a.judge, a.options.Steps)
			if err != nil {
				return Report{}, err
			}
			c.Forks++
			switch {
			case outcome.Error != "":
				c.Errored++
			case outcome.Passed == nil:
				c.Incomplete++
			case *outcome.Passed:
				c.Changed++
			}
			c.Evidence = append(c.Evidence, outcome)
		}
		c.Summary = summarize(c)
		report.Candidates = append(report.Candidates, c)
	}
	sort.SliceStable(report.Candidates, func(i, j int) bool {
		x, y := report.Candidates[i], report.Candidates[j]
		if x.Changed*y.Forks != y.Changed*x.Forks {
			return x.Changed*y.Forks > y.Changed*x.Forks
		}
		if x.Changed != y.Changed {
			return x.Changed > y.Changed
		}
		if x.EventSeq != y.EventSeq {
			return x.EventSeq < y.EventSeq
		}
		return x.Intervention < y.Intervention
	})
	return report, nil
}

// discoverCalls reads tool calls from a ledger already validated by
// checkpoint.List, where event i has sequence i+1.
func discoverCalls(events []store.Event) ([]call, error) {
	var calls []call
	decisionFor := -1
	for i, event := range events {
		switch event.Type {
		case "tool.request":
			var request struct {
				CallID      string `json:"call_id"`
				OperationID string `json:"operation_id"`
			}
			if err := json.Unmarshal(event.Payload, &request); err != nil {
				return nil, err
			}
			calls = append(calls, call{id: request.CallID, operation: request.OperationID, request: event.Seq})
		case "tool.response":
			calls[len(calls)-1].response = event.Seq
		case "model.request":
			decisionFor = -1
			if i > 0 && events[i-1].Type == "tool.response" {
				decisionFor = len(calls) - 1
			}
		case "model.response":
			if decisionFor >= 0 {
				calls[decisionFor].decision = event.Seq
			}
			decisionFor = -1
		}
	}
	return calls, nil
}

// planForks expands and validates every fork before any is created, so a bad
// target or unavailable provider writes nothing.
func planForks(ctx context.Context, source *store.Store, parent store.Run, manifest compiler.Manifest, set Set, calls []call, points []checkpoint.Checkpoint, options Options) ([]plan, error) {
	byID := map[string]call{}
	for _, c := range calls {
		byID[c.id] = c
	}
	_, _, chaosErr := source.ChaosPolicy(ctx, parent.ID)
	if chaosErr != nil && chaosErr != sql.ErrNoRows {
		return nil, chaosErr
	}
	var plans []plan
	for _, in := range set.interventions {
		targets := calls
		if in.Kind == KindToolResponse {
			in.Calls = []string{in.Call}
		}
		if in.Calls != nil {
			targets = nil
			for _, id := range in.Calls {
				c, ok := byID[id]
				if !ok {
					return nil, fmt.Errorf("intervention %s: run %s has no call %s", in.ID, parent.ID, id)
				}
				targets = append(targets, c)
			}
		}
		if in.Kind == KindFault && chaosErr == nil {
			return nil, fmt.Errorf("intervention %s: a legacy fault cannot be combined with the run's chaos policy", in.ID)
		}
		forkOptions := fork.Options{}
		switch in.Kind {
		case KindChaosPolicy:
			forkOptions.ChaosPolicyRaw = in.policyRaw
		case KindAuthPolicy:
			forkOptions.AuthPolicyRaw = in.policyRaw
		case KindFault:
			operation := ""
			if in.Operation != nil {
				operation = *in.Operation
			}
			forkOptions.FaultOperation = &operation
		case KindModel:
			forkOptions.Provider, forkOptions.Model = in.Provider, in.Model
		case KindToolResponse:
			forkOptions.Observation = &store.Observation{CallID: in.Call, Status: in.Status, Body: in.Body}
		}
		if err := fork.ValidateOptions(parent, manifest, forkOptions); err != nil {
			return nil, fmt.Errorf("intervention %s: %w", in.ID, err)
		}
		providerName, model := parent.Provider, parent.Model
		if forkOptions.Provider != "" {
			providerName = forkOptions.Provider
		}
		if forkOptions.Model != "" {
			model = forkOptions.Model
		}
		provider, err := options.ProviderFor(providerName, model, parent.Scenario)
		if err != nil {
			return nil, fmt.Errorf("intervention %s: %w", in.ID, err)
		}
		for _, c := range targets {
			candidate := Candidate{CallID: c.id, OperationID: c.operation, Intervention: in.ID, Kind: in.Kind}
			switch in.Kind {
			case KindModel:
				if c.decision == 0 {
					if in.Calls != nil {
						return nil, fmt.Errorf("intervention %s: no model decision follows call %s", in.ID, c.id)
					}
					continue
				}
				candidate.EventSeq, candidate.EventType, candidate.CheckpointSeq = c.decision, "model.response", c.response
				candidate.Label = "model decision after " + c.operation
			case KindToolResponse:
				candidate.EventSeq, candidate.EventType, candidate.CheckpointSeq = c.response, "tool.response", c.response
				candidate.Label = c.operation + " observation"
			default:
				candidate.EventSeq, candidate.EventType, candidate.CheckpointSeq = c.request, "tool.request", c.request-1
				candidate.Label = c.operation + " dispatch"
			}
			selected, err := checkpoint.Select(points, candidate.CheckpointSeq)
			if err != nil {
				return nil, fmt.Errorf("intervention %s: %w", in.ID, err)
			}
			plans = append(plans, plan{candidate: candidate, checkpoint: selected, options: forkOptions, provider: provider})
		}
	}
	if len(plans) == 0 {
		return nil, fmt.Errorf("interventions produced no candidate events in run %s", parent.ID)
	}
	return plans, nil
}

// runFork creates and executes one child. A failed child execution is recorded
// as evidence; store and evaluation failures abort the analysis.
func runFork(ctx context.Context, source, destination *store.Store, manifest compiler.Manifest, p plan, judge Judge, steps int) (Outcome, error) {
	created, err := fork.Create(ctx, source, destination, p.checkpoint, manifest, p.options)
	if err != nil {
		return Outcome{}, fmt.Errorf("intervention %s at event %d: %w", p.candidate.Intervention, p.candidate.EventSeq, err)
	}
	runner := agent.Runner{Store: destination, Dispatch: &dispatch.Dispatcher{Store: destination, Manifest: manifest}, Manifest: manifest, Provider: p.provider}
	child, runErr := runner.Execute(ctx, created.Run.ID, steps)
	if ctx.Err() != nil {
		return Outcome{}, ctx.Err()
	}
	outcome := Outcome{RunID: created.Run.ID}
	if runErr != nil {
		outcome.Status, outcome.Error = "failed", runErr.Error()
		return outcome, nil
	}
	outcome.Status = child.Status
	if child.Status != "completed" {
		return outcome, nil
	}
	passed, failed, err := judge.check(ctx, destination, child)
	if err != nil {
		return Outcome{}, err
	}
	outcome.Passed, outcome.Failed = &passed, failed
	return outcome, nil
}

func summarize(c Candidate) string {
	head := fmt.Sprintf("#%d %s [%s]", c.EventSeq, c.Label, c.Intervention)
	var text string
	if c.Changed > 0 {
		text = fmt.Sprintf("%s: corrected the final outcome in %d/%d forks", head, c.Changed, c.Forks)
	} else {
		text = fmt.Sprintf("%s: no material effect (0/%d forks corrected the outcome)", head, c.Forks)
	}
	if c.Errored > 0 || c.Incomplete > 0 {
		text += fmt.Sprintf("; %d errored, %d did not complete", c.Errored, c.Incomplete)
	}
	return text
}
