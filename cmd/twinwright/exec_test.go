package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"twinwright/internal/agent"
	"twinwright/internal/agent/exectest"
)

// TestMain turns this test binary into the agent process of the exec tests when
// the provider starts it with exectest.Mode set.
func TestMain(m *testing.M) {
	exectest.MaybeRun()
	os.Exit(m.Run())
}

func TestSplitAgentCommand(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string
	}{
		{"one word", "agent", []string{"agent"}},
		{"words", "python3 agent.py --fast", []string{"python3", "agent.py", "--fast"}},
		{"runs of blanks", "  python3 \t agent.py \n", []string{"python3", "agent.py"}},
		{"double quotes group", `python3 "my agent.py"`, []string{"python3", "my agent.py"}},
		{"single quotes group", `python3 'my agent.py'`, []string{"python3", "my agent.py"}},
		{"the other quote is literal", `"it's" 'say "hi"'`, []string{"it's", `say "hi"`}},
		{"quoted and bare text join", `a"b c"d e`, []string{"ab cd", "e"}},
		{"an empty quoted word stays", `agent ""`, []string{"agent", ""}},
		{"backslash is not an escape", `C:\tools\agent.exe a\ b`, []string{`C:\tools\agent.exe`, `a\`, "b"}},
		{"no variables or globs", `$HOME *.py ~ $(id) a;b|c`, []string{"$HOME", "*.py", "~", "$(id)", "a;b|c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := splitAgentCommand(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("splitAgentCommand(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	for _, in := range []string{"", "   ", "\t\n", `"open`, `'open`, `agent "open`, `agent 'it"s`} {
		if got, err := splitAgentCommand(in); err == nil {
			t.Errorf("splitAgentCommand(%q) = %q, want an error", in, got)
		}
	}
}

func TestProviderFlagsRegisterTheAgentFlags(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cfg := providerFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if cfg.AgentCommand != "" || len(cfg.AgentEnv) != 0 {
		t.Fatalf("defaults = %q %q, want none", cfg.AgentCommand, cfg.AgentEnv)
	}
	fs = flag.NewFlagSet("t", flag.ContinueOnError)
	cfg = providerFlags(fs)
	if err := fs.Parse([]string{"--agent-command", "python3 a.py", "--agent-env", "ONE", "--agent-env", "TWO"}); err != nil {
		t.Fatal(err)
	}
	if cfg.AgentCommand != "python3 a.py" || !slices.Equal(cfg.AgentEnv, []string{"ONE", "TWO"}) {
		t.Fatalf("parsed = %q %q", cfg.AgentCommand, cfg.AgentEnv)
	}
}

func TestSelectProviderBuildsTheExecProvider(t *testing.T) {
	t.Setenv("TW_AGENT_TOKEN", "token-value-for-test")
	t.Setenv("TW_AGENT_MODE", "fast")
	provider, err := selectProvider(providerConfig{
		Name: "exec", Model: "my-agent@1.2", Scenario: "duplicate-charge", Timeout: 7 * time.Second,
		AgentCommand: `python3 "agent dir/agent.py"`, AgentEnv: []string{"TW_AGENT_TOKEN", "TW_AGENT_MODE", "TW_AGENT_TOKEN"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := provider.(agent.ExecProvider)
	if !ok {
		t.Fatalf("provider = %T", provider)
	}
	if !slices.Equal(got.Command, []string{"python3", "agent dir/agent.py"}) {
		t.Errorf("command = %q", got.Command)
	}
	if want := []string{"TW_AGENT_TOKEN=token-value-for-test", "TW_AGENT_MODE=fast"}; !slices.Equal(got.Env, want) {
		t.Errorf("env = %q, want each name once, with the CLI's own value: %q", got.Env, want)
	}
	if got.Timeout != 7*time.Second {
		t.Errorf("timeout = %v", got.Timeout)
	}
}

func TestSelectProviderRefusesBadAgentConfigurations(t *testing.T) {
	t.Setenv("TW_AGENT_TOKEN", "token-value-for-test")
	t.Setenv("OPENAI_API_KEY", "test-key-not-a-secret")
	t.Setenv("TW_AGENT_UNSET", "")
	os.Unsetenv("TW_AGENT_UNSET")
	exec := providerConfig{Name: "exec", Model: "my-agent@1", Scenario: "duplicate-charge", Timeout: time.Second, AgentCommand: "agent"}
	with := func(edit func(*providerConfig)) providerConfig {
		cfg := exec
		edit(&cfg)
		return cfg
	}
	cases := []struct {
		name     string
		cfg      providerConfig
		want     []string
		unwanted string
	}{
		{"no command", with(func(c *providerConfig) { c.AgentCommand = "" }), []string{"--agent-command"}, ""},
		{"a command that does not split", with(func(c *providerConfig) { c.AgentCommand = `agent "open` }), []string{"--agent-command", "quote"}, ""},
		{"no model", with(func(c *providerConfig) { c.Model = "" }), []string{"--model", "agent", "version"}, ""},
		{"a value on the command line", with(func(c *providerConfig) { c.AgentEnv = []string{"TW_AGENT_TOKEN=hunter2-the-value"} }), []string{"--agent-env", "TW_AGENT_TOKEN"}, "hunter2-the-value"},
		{"an invalid name", with(func(c *providerConfig) { c.AgentEnv = []string{"NOT A NAME"} }), []string{"--agent-env"}, ""},
		{"a variable the CLI does not have", with(func(c *providerConfig) { c.AgentEnv = []string{"TW_AGENT_UNSET"} }), []string{"--agent-env", "TW_AGENT_UNSET"}, ""},
		{"a command for scripted", providerConfig{Name: "scripted", Scenario: "duplicate-charge", Timeout: time.Second, AgentCommand: "agent"}, []string{"--agent-command", "exec"}, ""},
		{"an env for openai", providerConfig{Name: "openai", Model: "gpt-test", Scenario: "duplicate-charge", Timeout: time.Second, AgentEnv: []string{"TW_AGENT_TOKEN"}}, []string{"--agent-env", "exec"}, ""},
		{"a cassette for exec", with(func(c *providerConfig) { c.Cassette = "x.jsonl" }), []string{"--cassette", "exec"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := selectProvider(tc.cfg)
			if err == nil {
				t.Fatal("accepted")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			if tc.unwanted != "" && strings.Contains(err.Error(), tc.unwanted) {
				t.Errorf("error %q repeats a value it must not", err)
			}
		})
	}
	record := exec
	record.RecordCassette = filepath.Join(t.TempDir(), "x.jsonl")
	if _, closer, err := openProvider(record); err == nil || !strings.Contains(err.Error(), "--record-cassette") {
		if closer != nil {
			closer.Close()
		}
		t.Fatalf("err = %v, want --record-cassette refused for exec", err)
	}
}

// The command and the environment are the operator's, not the run's, so a run
// started elsewhere cannot be continued without them, and the error says which
// flag is missing. The same refusal is what a worker meets for such a run.
func TestResumeOfExecRunNeedsTheCommand(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "billing")
	dir := t.TempDir()
	manifest, db := filepath.Join(dir, "manifest.json"), filepath.Join(dir, "world.db")
	invoke := func(args ...string) map[string]any {
		t.Helper()
		var out bytes.Buffer
		if err := runCLI(args, &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		var value map[string]any
		if err := json.Unmarshal(out.Bytes(), &value); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return value
	}
	invoke("build", filepath.Join(root, "openapi.yaml"), "--bindings", filepath.Join(root, "bindings.yaml"), "--out", manifest)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The mode is set in this process and handed to the child by name only.
	t.Setenv(exectest.Mode, "billing")
	command := `"` + exe + `"`

	// Preflight: no database is created for a run that could never start.
	if err := runCLI([]string{"run", "duplicate-charge", "--agent", "exec", "--agent-command", command, "--manifest", manifest, "--db", db}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "--model") {
		t.Fatalf("exec without --model: err = %v", err)
	}
	if _, err := os.Stat(db); !os.IsNotExist(err) {
		t.Fatalf("a refused run created a database: %v", err)
	}

	paused := invoke("run", "duplicate-charge", "--agent", "exec", "--model", "helper@test", "--agent-command", command, "--agent-env", exectest.Mode,
		"--manifest", manifest, "--db", db, "--steps", "2")
	run := paused["run"].(map[string]any)
	if run["status"] != "paused" || run["provider"] != "exec" || run["model"] != "helper@test" {
		t.Fatalf("run = %v", run)
	}
	id := run["id"].(string)

	var out bytes.Buffer
	err = runCLI([]string{"resume", id, "--manifest", manifest, "--db", db}, &out)
	if err == nil || !strings.Contains(err.Error(), "--agent-command") {
		t.Fatalf("resume without the command: err = %v, want it to name --agent-command", err)
	}
	completed := invoke("resume", id, "--agent-command", command, "--agent-env", exectest.Mode, "--manifest", manifest, "--db", db, "--steps", "20")
	if completed["evaluation"].(map[string]any)["passed"] != true {
		t.Fatalf("completed = %v", completed)
	}
	invoke("replay", id, "--manifest", manifest, "--db", db)

	// A worker meets the same refusal when it claims such a run.
	if _, err := selectProvider(providerConfig{Name: "exec", Model: "helper@test", Scenario: "duplicate-charge", Timeout: time.Second}); err == nil || !strings.Contains(err.Error(), "--agent-command") {
		t.Fatalf("worker-style selection without the command: err = %v", err)
	}
}
