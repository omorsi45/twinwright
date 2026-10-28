package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"twinwright/internal/agent/exectest"
)

// The agent flags describe an exec agent. A command that serves runs it did not
// start (worker, resume, fork) meets runs of every provider, so the flags apply
// to the runs whose recorded provider is exec and are dropped for the rest.
func TestForRunAppliesTheAgentFlagsOnlyToExecRuns(t *testing.T) {
	cfg := providerConfig{
		Timeout:      time.Second,
		AgentCommand: "agent", AgentEnv: []string{"TW_AGENT_TOKEN"}, AgentContainer: "python:3.12-alpine", AgentMount: "some/dir",
		AgentNetwork: "none", AgentMemory: "512m", AgentCPUs: "1", AgentPIDs: 128, BaseURL: "http://127.0.0.1:1/v1",
	}
	exec := cfg.forRun("exec", "my-agent@1", "duplicate-charge")
	if exec.AgentCommand != "agent" || len(exec.AgentEnv) != 1 || exec.AgentContainer == "" || exec.AgentMount == "" {
		t.Errorf("an exec run lost its agent flags: %+v", exec)
	}
	for _, name := range []string{"scripted", "openai", "openai-compatible", "anthropic"} {
		other := cfg.forRun(name, "m", "duplicate-charge")
		if other.AgentCommand != "" || len(other.AgentEnv) != 0 || other.AgentContainer != "" || other.AgentMount != "" {
			t.Errorf("a %s run kept agent flags: %+v", name, other)
		}
		if other.Name != name || other.Model != "m" || other.Scenario != "duplicate-charge" || other.BaseURL != cfg.BaseURL {
			t.Errorf("a %s run lost its other settings: %+v", name, other)
		}
	}
	if cfg.AgentCommand != "agent" || len(cfg.AgentEnv) != 1 {
		t.Errorf("forRun changed the shared config: %+v", cfg)
	}
	// A command that names the provider itself still refuses a mismatch.
	if _, err := selectProvider(cfg.with("scripted", "", "duplicate-charge")); err == nil || !strings.Contains(err.Error(), "--agent-command") {
		t.Errorf("a named non-exec provider with --agent-command: err = %v, want it refused", err)
	}
}

// queueRun creates a run with the given extra arguments and leaves it queued.
func queueRun(t *testing.T, manifest, db string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	all := append([]string{"run", "duplicate-charge", "--manifest", manifest, "--db", db, "--enqueue", "--steps", "20"}, args...)
	if err := runCLI(all, &out); err != nil {
		t.Fatalf("%v: %v\n%s", all, err, out.String())
	}
	var queued struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(out.Bytes(), &queued); err != nil || queued.RunID == "" {
		t.Fatalf("enqueue output: %v %s", err, out.String())
	}
	return queued.RunID
}

// One fleet serves scripted runs and exec runs from the same queue. A worker
// that is given the agent command so that it can serve the exec run must still
// serve the scripted one: the flags are for the runs that need them.
func TestWorkerWithAgentFlagsServesAScriptedRun(t *testing.T) {
	dir := t.TempDir()
	manifest := buildBillingManifest(t, dir)
	db := filepath.Join(dir, "world.db")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The mode is set in this process and handed to the agent by name only.
	t.Setenv(exectest.Mode, "billing")
	command := `"` + exe + `"`

	scripted := queueRun(t, manifest, db, "--agent", "scripted", "--seed", "11")
	execRun := queueRun(t, manifest, db, "--agent", "exec", "--model", "helper@test", "--agent-command", command, "--agent-env", exectest.Mode, "--seed", "12")

	var served bytes.Buffer
	if err := runCLI([]string{
		"worker", "--manifest", manifest, "--db", db, "--id", "mixed-worker", "--drain", "--lease-ttl", "60s",
		"--backoff", "10ms", "--max-attempts", "2",
		"--agent-command", command, "--agent-env", exectest.Mode,
	}, &served); err != nil {
		t.Fatalf("worker --drain: %v\n%s", err, served.String())
	}
	var result struct {
		Claims   int `json:"claims"`
		Outcomes []struct {
			RunID       string `json:"run_id"`
			Disposition string `json:"disposition"`
			RunStatus   string `json:"run_status"`
		} `json:"outcomes"`
	}
	if err := json.Unmarshal(served.Bytes(), &result); err != nil {
		t.Fatalf("decode worker output: %v\n%s", err, served.String())
	}
	status := map[string]string{}
	for _, outcome := range result.Outcomes {
		status[outcome.RunID] = outcome.Disposition + "/" + outcome.RunStatus
	}
	for name, id := range map[string]string{"scripted": scripted, "exec": execRun} {
		if status[id] != "finished/completed" {
			t.Errorf("the %s run %s ended as %q, want finished/completed\n%s", name, id, status[id], served.String())
		}
	}
}

// resume takes the provider from the run, so the agent flags are for exec runs
// only there too.
func TestResumeOfAScriptedRunIgnoresTheAgentFlags(t *testing.T) {
	dir := t.TempDir()
	manifest := buildBillingManifest(t, dir)
	db := filepath.Join(dir, "world.db")
	var out bytes.Buffer
	if err := runCLI([]string{"run", "duplicate-charge", "--agent", "scripted", "--manifest", manifest, "--db", db, "--steps", "2"}, &out); err != nil {
		t.Fatal(err)
	}
	var paused struct {
		Run struct {
			ID, Status string
		} `json:"run"`
	}
	if err := json.Unmarshal(out.Bytes(), &paused); err != nil || paused.Run.Status != "paused" {
		t.Fatalf("run output: %v %s", err, out.String())
	}
	out.Reset()
	if err := runCLI([]string{"resume", paused.Run.ID, "--agent-command", "some-agent", "--manifest", manifest, "--db", db, "--steps", "20"}, &out); err != nil {
		t.Fatalf("resume of a scripted run with an agent command: %v", err)
	}
	var resumed struct {
		Run struct {
			Status string `json:"status"`
		} `json:"run"`
		Evaluation struct {
			Passed bool `json:"passed"`
		} `json:"evaluation"`
	}
	if err := json.Unmarshal(out.Bytes(), &resumed); err != nil || resumed.Run.Status != "completed" || !resumed.Evaluation.Passed {
		t.Fatalf("the run did not complete and pass: %v\n%.400s", err, out.String())
	}
}

// A command that picks the provider itself keeps the refusal: the operator named
// a provider and an agent that will not be used, and silence would let them
// believe their agent ran.
func TestRunRefusesAgentFlagsForANonExecProvider(t *testing.T) {
	dir := t.TempDir()
	manifest := buildBillingManifest(t, dir)
	db := filepath.Join(dir, "world.db")
	err := runCLI([]string{"run", "duplicate-charge", "--agent", "scripted", "--agent-command", "some-agent", "--manifest", manifest, "--db", db}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--agent-command") {
		t.Fatalf("err = %v, want --agent-command refused for scripted", err)
	}
	if _, statErr := os.Stat(db); !os.IsNotExist(statErr) {
		t.Fatalf("a refused run created a database: %v", statErr)
	}
}

// fork takes the provider from the parent run unless --agent names another, and
// counterfactual from the run it analyses, so both apply the agent flags to an
// exec parent only.
func TestForkAndCounterfactualOnAScriptedRunIgnoreTheAgentFlags(t *testing.T) {
	examples := filepath.Join("..", "..", "examples")
	dir := t.TempDir()
	manifest := buildBillingManifest(t, dir)
	db := filepath.Join(dir, "world.db")
	invoke := func(t *testing.T, args ...string) []byte {
		t.Helper()
		var out bytes.Buffer
		if err := runCLI(args, &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.Bytes()
	}
	var root struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(invoke(t, "run", "ambiguous-commit", "--agent", "scripted", "--recovery", "unsafe", "--manifest", manifest, "--db", db,
		"--chaos", filepath.Join(examples, "chaos", "ambiguous-commit.yaml")), &root); err != nil {
		t.Fatal(err)
	}

	var points struct {
		Checkpoints []struct {
			EventSeq  int    `json:"event_seq"`
			EventType string `json:"event_type"`
		} `json:"checkpoints"`
	}
	if err := json.Unmarshal(invoke(t, "checkpoints", root.Run.ID, "--manifest", manifest, "--db", db), &points); err != nil {
		t.Fatal(err)
	}
	seq := 0
	for _, point := range points.Checkpoints {
		if point.EventType == "tool.response" {
			seq = point.EventSeq
			break
		}
	}
	if seq == 0 {
		t.Fatal("missing tool response checkpoint")
	}

	t.Run("fork inherits the parent's provider", func(t *testing.T) {
		forked := invoke(t, "fork", root.Run.ID, "--at-event", strconv.Itoa(seq), "--steps", "20", "--agent-command", "some-agent", "--agent-env", "PATH", "--manifest", manifest, "--db", db)
		if !strings.Contains(string(forked), `"fork"`) {
			t.Fatalf("fork output: %.300s", forked)
		}
	})
	t.Run("counterfactual uses the run's provider", func(t *testing.T) {
		counterfactual := invoke(t, "counterfactual", root.Run.ID, "--interventions", filepath.Join(examples, "counterfactual", "ambiguous-commit.yaml"),
			"--agent-command", "some-agent", "--manifest", manifest, "--db", db)
		if !strings.Contains(string(counterfactual), `"candidates"`) {
			t.Fatalf("counterfactual output: %.300s", counterfactual)
		}
	})
	// Naming the provider on the command line is the operator's choice, so agent
	// flags that cannot apply to it are refused.
	t.Run("fork --agent keeps the refusal", func(t *testing.T) {
		err := runCLI([]string{"fork", root.Run.ID, "--at-event", strconv.Itoa(seq), "--steps", "20", "--agent", "openai-compatible", "--model", "local",
			"--base-url", "http://127.0.0.1:1/v1", "--agent-command", "some-agent", "--manifest", manifest, "--db", db}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "--agent-command") {
			t.Fatalf("fork --agent openai-compatible --agent-command: err = %v, want it refused", err)
		}
	})
}
