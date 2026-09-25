package shadow

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"twinwright/internal/agent"
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
}

// Simulate runs the agent against a local SQLite world and returns proposed tool calls.
func Simulate(ctx context.Context, options SimulateOptions) ([]ProposedAction, string, error) {
	if options.Provider == nil {
		return nil, "", fmt.Errorf("provider is required")
	}
	if options.Steps < 1 {
		return nil, "", fmt.Errorf("steps must be positive")
	}
	if options.WorkDir == "" {
		return nil, "", fmt.Errorf("work dir is required")
	}
	if err := os.MkdirAll(options.WorkDir, 0755); err != nil {
		return nil, "", err
	}
	dbPath := filepath.Join(options.WorkDir, "shadow.db")
	s, err := store.Open(dbPath)
	if err != nil {
		return nil, "", err
	}
	defer s.Close()
	seed := options.Seed
	if seed == 0 {
		seed = 42
	}
	world, err := s.SeedScenario(ctx, seed, options.Manifest.Digest, options.Scenario)
	if err != nil {
		return nil, "", err
	}
	run, err := s.CreateRunConfigured(ctx, world.ID, options.Scenario, options.AgentName, options.Model, options.Task, store.RunOptions{})
	if err != nil {
		return nil, "", err
	}
	runner := agent.Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: options.Manifest}, Manifest: options.Manifest, Provider: options.Provider}
	run, err = runner.Execute(ctx, run.ID, options.Steps)
	if err != nil && run.Status != "completed" && run.Status != "paused" {
		return nil, run.ID, err
	}
	var history []agent.Message
	if err := json.Unmarshal([]byte(run.Transcript), &history); err != nil {
		return nil, run.ID, err
	}
	var proposed []ProposedAction
	for _, m := range history {
		if m.Role != "assistant" {
			continue
		}
		for _, call := range m.ToolCalls {
			proposed = append(proposed, ProposedAction{CallID: call.ID, OperationID: call.OperationID, Arguments: call.Arguments})
		}
	}
	return proposed, run.ID, nil
}

// Comparison is a measurement of proposed vs observed actions.
type Comparison struct {
	Matched       []string `json:"matched"`
	OnlyProposed  []string `json:"only_proposed"`
	OnlyObserved  []string `json:"only_observed"`
	ProposedCount int      `json:"proposed_count"`
	ObservedCount int      `json:"observed_count"`
}

// Compare pairs actions by operation_id and canonical arguments. It does not declare a winner.
func Compare(proposed []ProposedAction, observed []Observation) (Comparison, error) {
	type key struct{ op, args string }
	canon := func(op string, args map[string]any) (key, error) {
		data, err := json.Marshal(args)
		if err != nil {
			return key{}, err
		}
		return key{op: op, args: string(data)}, nil
	}
	obsKeys := map[key]int{}
	for _, o := range observed {
		k, err := canon(o.OperationID, o.Arguments)
		if err != nil {
			return Comparison{}, err
		}
		obsKeys[k]++
	}
	propKeys := map[key]int{}
	for _, p := range proposed {
		k, err := canon(p.OperationID, p.Arguments)
		if err != nil {
			return Comparison{}, err
		}
		propKeys[k]++
	}
	out := Comparison{ProposedCount: len(proposed), ObservedCount: len(observed)}
	for k, n := range propKeys {
		m := obsKeys[k]
		matched := n
		if m < matched {
			matched = m
		}
		label := k.op
		for i := 0; i < matched; i++ {
			out.Matched = append(out.Matched, label)
		}
		for i := 0; i < n-matched; i++ {
			out.OnlyProposed = append(out.OnlyProposed, label)
		}
	}
	for k, n := range obsKeys {
		m := propKeys[k]
		if n > m {
			for i := 0; i < n-m; i++ {
				out.OnlyObserved = append(out.OnlyObserved, k.op)
			}
		}
	}
	return out, nil
}
