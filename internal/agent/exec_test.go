package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"twinwright/internal/agent/exectest"
	"twinwright/internal/compiler"
	"twinwright/internal/dispatch"
	"twinwright/internal/eval"
	"twinwright/internal/store"
)

// TestMain turns this test binary into the agent process when the exec provider
// starts it with exectest.Mode set. Without the variable it is an ordinary
// test run.
func TestMain(m *testing.M) {
	exectest.MaybeRun()
	os.Exit(m.Run())
}

// helperProvider is an ExecProvider whose agent is this test binary in the
// named mode. extra holds further NAME=value pairs for the child.
func helperProvider(t testing.TB, mode string, extra ...string) ExecProvider {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return ExecProvider{
		Command: []string{exe},
		Env:     append([]string{exectest.Mode + "=" + mode}, extra...),
		Timeout: 30 * time.Second,
	}
}

func execBillingManifest(t testing.TB) compiler.Manifest {
	t.Helper()
	root := filepath.Join("..", "..", "examples", "billing")
	spec, err := os.ReadFile(filepath.Join(root, "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := os.ReadFile(filepath.Join(root, "bindings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := compiler.Compile(spec, bindings)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

// completeBillingRun drives provider through the duplicate-charge scenario with
// the charge listing failing once, and returns the finished run with its
// evaluation. It is the check that an agent process does the actual job, not
// merely that it answers.
func completeBillingRun(t *testing.T, provider Provider) (store.Run, eval.Report) {
	t.Helper()
	ctx := context.Background()
	manifest := execBillingManifest(t)
	s, err := store.Open(filepath.Join(t.TempDir(), "world.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	world, err := s.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "exec", "reference@test", "Customer C-104 was charged twice. Refund only the duplicate.", "listCharges")
	if err != nil {
		t.Fatal(err)
	}
	runner := Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: provider}
	done, err := runner.Execute(ctx, run.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	report, err := eval.DuplicateCharge(ctx, s, world.ID)
	if err != nil {
		t.Fatal(err)
	}
	return done, report
}

func TestExecProviderRejectsMisbehavingAgents(t *testing.T) {
	const secret = "exec-test-secret-value-1234"
	cases := []struct {
		name, mode string
		timeout    time.Duration
		within     time.Duration
		want       []string
		// interrupted is a timeout the runner may retry; every other failure is
		// an ordinary provider error.
		interrupted bool
		// gone means the agent process must not outlive the call.
		gone bool
		// children means the agent started a process of its own, whose pid is
		// in the file next to the agent's, and that must be gone too.
		children bool
	}{
		{name: "not JSON", mode: "non-json", want: []string{"not a valid JSON object"}},
		{name: "no output", mode: "empty", want: []string{"no output"}},
		{name: "exit 3 keeps the stderr tail, masked", mode: "exit3", want: []string{"exited with status 3", "agent exploded", "[REDACTED]"}},
		{name: "unknown response field", mode: "unknown-field", want: []string{"unknown field", "surprise"}},
		{name: "wrong protocol", mode: "wrong-protocol", want: []string{"protocol", "twinwright.agent.v2", AgentProtocol}},
		{name: "no protocol", mode: "no-protocol", want: []string{"protocol", AgentProtocol}},
		{name: "no tool_calls", mode: "no-tool-calls", want: []string{"tool_calls"}},
		{name: "no content", mode: "no-content", want: []string{"content"}},
		{name: "neither content nor calls", mode: "nothing-to-say", want: []string{"no content and no tool calls"}},
		{name: "data after the object", mode: "trailing", want: []string{"after the response"}},
		{name: "stdout flood", mode: "flood", within: 10 * time.Second, want: []string{"exceeded", strconv.Itoa(MaxAgentOutputBytes)}, gone: true},
		{name: "hang", mode: "hang", timeout: 500 * time.Millisecond, within: 2 * time.Second, interrupted: true, gone: true},
		{name: "hang that ignores SIGTERM", mode: "hang-ignore-term", timeout: 500 * time.Millisecond, within: 4 * time.Second, interrupted: true, gone: true},
		{name: "hang with a child that ignores SIGTERM", mode: "spawn-hang", timeout: 500 * time.Millisecond, within: 4 * time.Second, interrupted: true, gone: true, children: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			p := helperProvider(t, tc.mode, exectest.PIDFile+"="+pidFile, exectest.Secret+"="+secret)
			if tc.timeout > 0 {
				p.Timeout = tc.timeout
			}
			within := tc.within
			if within == 0 {
				within = 10 * time.Second
			}
			start := time.Now()
			_, err := p.Next(context.Background(), "task", nil, nil)
			elapsed := time.Since(start)
			if err == nil {
				t.Fatal("a misbehaving agent was accepted")
			}
			if elapsed > within {
				t.Fatalf("took %v, want under %v", elapsed, within)
			}
			var interrupt *CallInterrupted
			isInterrupt := errors.As(err, &interrupt)
			if tc.interrupted != isInterrupt || (isInterrupt && interrupt.Kind != InterruptTimeout) {
				t.Fatalf("interrupted = %v (%+v), want %v: %v", isInterrupt, interrupt, tc.interrupted, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error leaks a passed secret: %q", err)
			}
			if tc.gone {
				requireGone(t, pidFile, 0)
				if tc.children {
					// Not this process's child, so it is reaped by init, which may
					// take a moment after the kill.
					requireGone(t, pidFile+exectest.ChildSuffix, 3*time.Second)
				}
			}
		})
	}
}

// requireGone fails unless the process whose pid is in file has ended, waiting
// up to patience for it to be reaped.
func requireGone(t *testing.T, file string, patience time.Duration) {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("the process never wrote its pid, so its exit cannot be checked: %v", err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	for end := time.Now().Add(patience); processAlive(pid); {
		if time.Now().After(end) {
			t.Fatalf("process %d outlived the call", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestExecProviderRefusesAnUnstartableAgent(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-agent")
	_, err := ExecProvider{Command: []string{missing}, Timeout: time.Second}.Next(context.Background(), "task", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "start") {
		t.Fatalf("err = %v, want a start failure", err)
	}
	var interrupt *CallInterrupted
	if errors.As(err, &interrupt) {
		t.Fatalf("a missing binary is not transient, but was classified %+v", interrupt)
	}
}

func TestExecProviderRefusesBadConfiguration(t *testing.T) {
	cases := []struct {
		name string
		p    ExecProvider
		want string
	}{
		{"no command", ExecProvider{}, "command"},
		{"empty program", ExecProvider{Command: []string{""}}, "command"},
		{"env entry without a value", ExecProvider{Command: []string{"agent"}, Env: []string{"NAME"}}, "NAME"},
		{"env entry without a name", ExecProvider{Command: []string{"agent"}, Env: []string{"=value"}}, "name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.p.Next(context.Background(), "task", nil, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// An agent that exits without reading its input must not hang the turn, even
// when the request is far larger than a pipe buffer: the write would otherwise
// block for ever.
func TestExecProviderDoesNotBlockOnAnAgentThatNeverReads(t *testing.T) {
	p := helperProvider(t, "ignore-stdin")
	p.Timeout = 10 * time.Second
	task := strings.Repeat("a", 4<<20)
	start := time.Now()
	message, err := p.Next(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if message.Role != "assistant" || message.Content != "done" {
		t.Fatalf("message = %+v", message)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %v", elapsed)
	}
}

func TestExecProviderKeepsOnlyTheTailOfStderr(t *testing.T) {
	_, err := helperProvider(t, "stderr-flood").Next(context.Background(), "task", nil, nil)
	if err == nil {
		t.Fatal("a failing agent was accepted")
	}
	if !strings.Contains(err.Error(), "END-OF-STDERR") {
		t.Fatalf("the end of stderr is missing from %.200q", err)
	}
	if len(err.Error()) > 8<<10 {
		t.Fatalf("error text is %d bytes; stderr is not bounded", len(err.Error()))
	}
}

func TestExecChildEnvironmentIsMinimal(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key-not-a-secret")
	t.Setenv("ANTHROPIC_API_KEY", "test-key-not-a-secret")
	t.Setenv("DATABASE_URL", "postgres://user:not-a-password@host/db")
	t.Setenv("HOME", "/home/exec-test")
	t.Setenv("PATH", os.Getenv("PATH"))
	message, err := helperProvider(t, "environ", "TW_PASSED=value").Next(context.Background(), "task", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(message.Content, ",")
	want := []string{"HOME", "PATH", "TW_EXEC_AGENT_HELPER", "TW_PASSED"}
	if !slices.Equal(got, want) {
		t.Fatalf("child environment names = %v, want exactly %v", got, want)
	}
}

func TestExecProviderMasksPassedSecretsInTheAnswer(t *testing.T) {
	const secret = "exec-test-secret-value-1234"
	message, err := helperProvider(t, "leak", exectest.Secret+"="+secret).Next(context.Background(), "task", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(message.Content, secret) || !strings.Contains(message.Content, "[REDACTED]") {
		t.Fatalf("content = %q, want the secret masked", message.Content)
	}
}

func TestExecRequestCarriesTheProtocol(t *testing.T) {
	manifest := execBillingManifest(t)
	raw := echoedRequest(t, nil, manifest.Operations)
	var request struct {
		Protocol   string            `json:"protocol"`
		Task       string            `json:"task"`
		History    []json.RawMessage `json:"history"`
		Operations []map[string]any  `json:"operations"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	if request.Protocol != AgentProtocol || request.Task != "the task" {
		t.Fatalf("request = %s", raw)
	}
	if request.History == nil || len(request.History) != 0 {
		t.Fatalf("an empty history must be [] and not null: %s", raw)
	}
	if len(request.Operations) != len(manifest.Operations) {
		t.Fatalf("%d operations sent, want %d", len(request.Operations), len(manifest.Operations))
	}
	for i, op := range manifest.Operations {
		sent := request.Operations[i]
		keys := make([]string, 0, len(sent))
		for key := range sent {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		if want := []string{"description", "method", "operation_id", "parameters", "path"}; !slices.Equal(keys, want) {
			t.Fatalf("operation %s has keys %v, want %v", op.ID, keys, want)
		}
		if sent["operation_id"] != op.ID || sent["method"] != op.Method || sent["path"] != op.Path || sent["description"] == "" {
			t.Fatalf("operation %s sent as %v", op.ID, sent)
		}
		parameters := sent["parameters"].(map[string]any)
		properties := parameters["properties"].(map[string]any)
		if parameters["type"] != "object" || len(properties) != len(op.Properties) || len(parameters["required"].([]any)) != len(op.Required) {
			t.Fatalf("operation %s parameters = %v, want %v required %v", op.ID, parameters, op.Properties, op.Required)
		}
		for name, typ := range op.Properties {
			if properties[name].(map[string]any)["type"] != typ {
				t.Fatalf("operation %s property %s = %v, want type %s", op.ID, name, properties[name], typ)
			}
		}
	}
}

// echoedRequest returns the exact bytes the agent process received.
func echoedRequest(t *testing.T, history []Message, ops []compiler.Operation) []byte {
	t.Helper()
	message, err := helperProvider(t, "echo-request").Next(context.Background(), "the task", history, ops)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(message.Content)
}

func TestExecProviderAnswersAsAnAssistantTurn(t *testing.T) {
	history := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "exec-1", OperationID: "getCustomer", Arguments: map[string]any{"id": "C-104"}}}},
		{Role: "tool", CallID: "exec-1", OperationID: "getCustomer", Status: 200, Content: `{"id":"C-104"}`},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "exec-2", OperationID: "listInvoices", Arguments: map[string]any{"id": "C-104"}}}},
		{Role: "tool", CallID: "exec-2", OperationID: "listInvoices", Status: 200, Content: `[]`},
	}
	message, err := helperProvider(t, "billing").Next(context.Background(), "task", history, nil)
	if err != nil {
		t.Fatal(err)
	}
	if message.Role != "assistant" || len(message.ToolCalls) != 1 {
		t.Fatalf("message = %+v", message)
	}
	call := message.ToolCalls[0]
	if call.ID != "exec-3" || call.OperationID != "listCharges" || fmt.Sprint(call.Arguments["id"]) != "INV-104" {
		t.Fatalf("call = %+v", call)
	}
}

func TestExecProviderRecordsUsageTheAgentReports(t *testing.T) {
	message, err := helperProvider(t, "with-usage").Next(context.Background(), "task", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "counted" || message.Usage == nil || *message.Usage != (Usage{InputTokens: 3, OutputTokens: 4, TotalTokens: 7}) {
		t.Fatalf("message = %+v", message)
	}
}

func TestPythonReferenceAgent(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "examples", "agents", "python", "billing_agent.py"))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("completes the billing run", func(t *testing.T) {
		run, report := completeBillingRun(t, ExecProvider{Command: []string{python, script}, Timeout: 30 * time.Second})
		if run.Status != "completed" || !report.Passed {
			t.Fatalf("run = %s, evaluation = %+v", run.Status, report)
		}
	})
	t.Run("echoes its request to stderr on demand", func(t *testing.T) {
		request, err := json.Marshal(execRequest{Protocol: AgentProtocol, Task: "task", History: []Message{}, Operations: []execOperation{}})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(python, script, "--echo-request")
		cmd.Stdin = bytes.NewReader(request)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%v: %s", err, stderr.String())
		}
		if !strings.Contains(stderr.String(), `"task"`) || !strings.Contains(stderr.String(), AgentProtocol) {
			t.Fatalf("stderr = %q, want the request", stderr.String())
		}
		var response execResponse
		if err := decodeExecResponse(stdout.Bytes(), &response); err != nil {
			t.Fatalf("stdout %q: %v", stdout.String(), err)
		}
	})
}

// requiredFields lists the JSON names of the fields of a struct that are always
// written, which is what a schema must mark required. A field marked omitempty
// is optional on the wire.
func requiredFields(t reflect.Type) []string {
	var names []string
	for i := range t.NumField() {
		name, options, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" || strings.Contains(options, "omitempty") {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// fieldNames lists every JSON name of a struct, optional or not.
func fieldNames(t reflect.Type) []string {
	var names []string
	for i := range t.NumField() {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "" && name != "-" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func loadSchema(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "protocol", name))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("%s is not JSON: %v", name, err)
	}
	if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("%s declares $schema %v, want draft 2020-12", name, schema["$schema"])
	}
	return schema
}

func schemaChild(t *testing.T, schema map[string]any, path ...string) map[string]any {
	t.Helper()
	node := schema
	for _, step := range path {
		next, ok := node[step].(map[string]any)
		if !ok {
			t.Fatalf("schema has no %q under %v", step, path)
		}
		node = next
	}
	return node
}

func stringList(t *testing.T, value any) []string {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("%v is not a list", value)
	}
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.(string)
	}
	slices.Sort(out)
	return out
}

// checkObjectSchema compares one object schema with the Go struct it describes:
// its required list must be the struct's always-written fields, and its
// properties must be every JSON field, no more and no fewer.
func checkObjectSchema(t *testing.T, label string, schema map[string]any, typ reflect.Type) {
	t.Helper()
	if got, want := stringList(t, schema["required"]), requiredFields(typ); !slices.Equal(got, want) {
		t.Errorf("%s required = %v, want %v", label, got, want)
	}
	properties := schema["properties"].(map[string]any)
	var got []string
	for name := range properties {
		got = append(got, name)
	}
	slices.Sort(got)
	if want := fieldNames(typ); !slices.Equal(got, want) {
		t.Errorf("%s properties = %v, want %v", label, got, want)
	}
}

func TestAgentSchemasMatchTheProtocol(t *testing.T) {
	request := loadSchema(t, "agent-v1-request.schema.json")
	response := loadSchema(t, "agent-v1-response.schema.json")

	checkObjectSchema(t, "request", request, reflect.TypeOf(execRequest{}))
	checkObjectSchema(t, "request operation", schemaChild(t, request, "properties", "operations", "items"), reflect.TypeOf(execOperation{}))
	checkObjectSchema(t, "request history message", schemaChild(t, request, "properties", "history", "items"), reflect.TypeOf(Message{}))
	checkObjectSchema(t, "request history tool call", schemaChild(t, request, "properties", "history", "items", "properties", "tool_calls", "items"), reflect.TypeOf(ToolCall{}))
	checkObjectSchema(t, "request history usage", schemaChild(t, request, "properties", "history", "items", "properties", "usage"), reflect.TypeOf(Usage{}))
	checkObjectSchema(t, "request history interruption", schemaChild(t, request, "properties", "history", "items", "properties", "interruptions", "items"), reflect.TypeOf(Interruption{}))
	checkObjectSchema(t, "response", response, reflect.TypeOf(execResponse{}))
	checkObjectSchema(t, "response tool call", schemaChild(t, response, "properties", "tool_calls", "items"), reflect.TypeOf(execToolCall{}))
	checkObjectSchema(t, "response usage", schemaChild(t, response, "properties", "usage"), reflect.TypeOf(Usage{}))
	if got := schemaChild(t, request, "properties", "protocol")["const"]; got != AgentProtocol {
		t.Errorf("request protocol const = %v", got)
	}
	if got := schemaChild(t, response, "properties", "protocol")["const"]; got != AgentProtocol {
		t.Errorf("response protocol const = %v", got)
	}
	if response["additionalProperties"] != false {
		t.Error("the response schema must refuse unknown fields, as the provider does")
	}

	// Examples: the request bytes an agent really receives, and the answers the
	// helper agent really gives to them.
	history := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "exec-1", OperationID: "getCustomer", Arguments: map[string]any{"id": "C-104"}}}},
		{Role: "tool", CallID: "exec-1", OperationID: "getCustomer", Status: 200, Content: `{"id":"C-104"}`},
	}
	sent := echoedRequest(t, history, execBillingManifest(t).Operations)
	answer, err := exectest.Respond(sent)
	if err != nil {
		t.Fatal(err)
	}
	final, err := exectest.Respond(mustJSON(t, map[string]any{"protocol": AgentProtocol, "task": "t", "history": toolHistory(), "operations": []any{}}))
	if err != nil {
		t.Fatal(err)
	}
	withUsage := []byte(`{"protocol":"` + AgentProtocol + `","content":"x","tool_calls":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)
	for label, doc := range map[string][]byte{"request": sent, "billing answer": answer, "final answer": final, "answer with usage": withUsage} {
		schema := response
		if label == "request" {
			schema = request
		}
		var value any
		if err := json.Unmarshal(doc, &value); err != nil {
			t.Fatal(err)
		}
		if err := validateSchema(schema, value, label); err != nil {
			t.Errorf("%s does not satisfy its schema: %v\n%s", label, err, doc)
		}
	}

	// The validator and the schemas must actually bite.
	for label, doc := range map[string]string{
		"no tool_calls":  `{"protocol":"` + AgentProtocol + `","content":"x"}`,
		"no content":     `{"protocol":"` + AgentProtocol + `","tool_calls":[]}`,
		"no protocol":    `{"content":"x","tool_calls":[]}`,
		"other protocol": `{"protocol":"twinwright.agent.v2","content":"x","tool_calls":[]}`,
		"unknown field":  `{"protocol":"` + AgentProtocol + `","content":"x","tool_calls":[],"extra":1}`,
		"bad tool call":  `{"protocol":"` + AgentProtocol + `","content":"","tool_calls":[{"id":"a","operation_id":"b"}]}`,
	} {
		var value any
		if err := json.Unmarshal([]byte(doc), &value); err != nil {
			t.Fatal(err)
		}
		if validateSchema(response, value, label) == nil {
			t.Errorf("%s was accepted by the response schema", label)
		}
	}
}

func toolHistory() []any {
	return []any{
		map[string]any{"role": "tool", "call_id": "exec-1", "operation_id": "createRefund", "status": 201, "content": "{}"},
		map[string]any{"role": "tool", "call_id": "exec-2", "operation_id": "createRefund", "status": 201, "content": "{}"},
		map[string]any{"role": "tool", "call_id": "exec-3", "operation_id": "createRefund", "status": 201, "content": "{}"},
		map[string]any{"role": "tool", "call_id": "exec-4", "operation_id": "createRefund", "status": 201, "content": "{}"},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// validateSchema checks value against the subset of JSON Schema the protocol
// schemas use: type, const, required, properties, additionalProperties: false
// and items. It is not a general validator; a keyword it does not know is an
// error, so a schema cannot quietly grow a constraint nothing checks.
func validateSchema(schema map[string]any, value any, path string) error {
	for keyword, argument := range schema {
		switch keyword {
		case "$schema", "$id", "title", "description", "examples", "properties", "items", "required", "additionalProperties", "type", "const":
		default:
			return fmt.Errorf("%s: schema keyword %q is not supported by this test", path, keyword)
		}
		switch keyword {
		case "type":
			if err := checkType(argument.(string), value, path); err != nil {
				return err
			}
		case "const":
			if value != argument {
				return fmt.Errorf("%s: %v is not the constant %v", path, value, argument)
			}
		case "required":
			object, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: required applies to an object", path)
			}
			for _, name := range argument.([]any) {
				if _, present := object[name.(string)]; !present {
					return fmt.Errorf("%s: missing required field %q", path, name)
				}
			}
		case "properties":
			object, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: properties applies to an object", path)
			}
			for name, child := range argument.(map[string]any) {
				if member, present := object[name]; present {
					if err := validateSchema(child.(map[string]any), member, path+"."+name); err != nil {
						return err
					}
				}
			}
		case "additionalProperties":
			if argument == false {
				known, _ := schema["properties"].(map[string]any)
				for name := range value.(map[string]any) {
					if _, ok := known[name]; !ok {
						return fmt.Errorf("%s: unknown field %q", path, name)
					}
				}
			}
		case "items":
			list, ok := value.([]any)
			if !ok {
				return fmt.Errorf("%s: items applies to an array", path)
			}
			for i, item := range list {
				if err := validateSchema(argument.(map[string]any), item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func checkType(want string, value any, path string) error {
	ok := false
	switch want {
	case "object":
		_, ok = value.(map[string]any)
	case "array":
		_, ok = value.([]any)
	case "string":
		_, ok = value.(string)
	case "boolean":
		_, ok = value.(bool)
	case "integer":
		number, isNumber := value.(float64)
		ok = isNumber && number == float64(int64(number))
	case "number":
		_, ok = value.(float64)
	default:
		return fmt.Errorf("%s: type %q is not supported by this test", path, want)
	}
	if !ok {
		return fmt.Errorf("%s: %v is not of type %s", path, value, want)
	}
	return nil
}
