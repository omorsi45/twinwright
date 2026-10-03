package shadow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"twinwright/internal/agent"
	"twinwright/internal/authz"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

// ProposedAction is one tool call the agent would make in the local world.
type ProposedAction struct {
	CallID      string         `json:"call_id"`
	OperationID string         `json:"operation_id"`
	Arguments   map[string]any `json:"arguments"`
}

// SimulateOptions drives a local, write-isolated simulation.
type SimulateOptions struct {
	Manifest  compiler.Manifest
	Scenario  string
	Task      string
	Provider  agent.Provider
	AgentName string
	Model     string
	Seed      int64
	Steps     int
	WorkDir   string
	// Policy is the principal policy the proposed actions are screened against.
	// Nil means no screen, which the comparison reports as unmeasured.
	Policy *authz.Policy
}

// Result is one shadow simulation's output.
type Result struct {
	RunID    string           `json:"run_id"`
	Proposed []ProposedAction `json:"proposed"`
	// Policy is nil when no policy was configured. An absent screen is not a
	// clean one, so the comparison reports the difference.
	Policy *PolicyScreen `json:"policy_screen,omitempty"`
}

// Simulate runs the agent against a local SQLite world and returns proposed tool calls.
func Simulate(ctx context.Context, options SimulateOptions) (Result, error) {
	if options.Provider == nil {
		return Result{}, fmt.Errorf("provider is required")
	}
	if options.Steps < 1 {
		return Result{}, fmt.Errorf("steps must be positive")
	}
	if options.WorkDir == "" {
		return Result{}, fmt.Errorf("work dir is required")
	}
	if err := os.MkdirAll(options.WorkDir, 0755); err != nil {
		return Result{}, err
	}
	dbPath := filepath.Join(options.WorkDir, "shadow.db")
	s, err := store.Open(dbPath)
	if err != nil {
		return Result{}, err
	}
	defer s.Close()
	seed := options.Seed
	if seed == 0 {
		seed = 42
	}
	world, err := s.SeedScenario(ctx, seed, options.Manifest.Digest, options.Scenario)
	if err != nil {
		return Result{}, err
	}
	run, err := s.CreateRunConfigured(ctx, world.ID, options.Scenario, options.AgentName, options.Model, options.Task, store.RunOptions{})
	if err != nil {
		return Result{}, err
	}
	runner := agent.Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: options.Manifest}, Manifest: options.Manifest, Provider: options.Provider}
	run, err = runner.Execute(ctx, run.ID, options.Steps)
	if err != nil && run.Status != "completed" && run.Status != "paused" {
		return Result{RunID: run.ID}, err
	}
	var history []agent.Message
	if err := json.Unmarshal([]byte(run.Transcript), &history); err != nil {
		return Result{RunID: run.ID}, err
	}
	result := Result{RunID: run.ID}
	for _, m := range history {
		if m.Role != "assistant" {
			continue
		}
		for _, call := range m.ToolCalls {
			result.Proposed = append(result.Proposed, ProposedAction{CallID: call.ID, OperationID: call.OperationID, Arguments: call.Arguments})
		}
	}
	if options.Policy != nil {
		// Screened here rather than in Compare because an accurate verdict needs
		// the world the calls would have landed in, and this is where it is open.
		screen, err := screenProposed(ctx, s.DB, world.ID, *options.Policy, options.Manifest, result.Proposed)
		if err != nil {
			return result, err
		}
		result.Policy = &screen
	}
	return result, nil
}

// screenProposed asks the policy what it would decide about each proposed call,
// in the order the agent proposed them.
//
// It calls the same authz entry point the dispatcher's Decide delegates to, so a
// shadow verdict cannot drift away from an enforced one. Nothing is executed and
// nothing is written: the screen reads the world to resolve resource scope and
// mirrors the dispatcher's rule that a call with arguments a handler would
// reject consumes no call number.
func screenProposed(ctx context.Context, world authz.Querier, worldID string, policy authz.Policy, manifest compiler.Manifest, proposed []ProposedAction) (PolicyScreen, error) {
	screen := PolicyScreen{Principal: policy.Principal.ID}
	call := 0
	for _, action := range proposed {
		op := manifest.Operation(action.OperationID)
		if op == nil {
			return PolicyScreen{}, fmt.Errorf("proposed action %s is not an operation in this world", action.OperationID)
		}
		decision, err := authz.Screen(ctx, world, worldID, policy, *op, action.Arguments, call+1)
		if err != nil {
			return PolicyScreen{}, err
		}
		screen.Screened++
		if decision.Invalid {
			// The handler, not the policy, refuses this one, and it is not an
			// authorization finding. Counting it keeps it out of the refusal
			// list without losing it.
			screen.InvalidArguments++
			continue
		}
		call++
		if decision.Allowed {
			continue
		}
		screen.Refusals = append(screen.Refusals, PolicyRefusal{
			OperationID: action.OperationID, Arguments: action.Arguments,
			Permission: decision.Permission, Reason: decision.Reason, Call: decision.Call,
		})
	}
	return screen, nil
}
