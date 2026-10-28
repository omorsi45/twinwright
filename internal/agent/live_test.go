package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"twinwright/internal/compiler"
)

// Live provider tests.
//
// These are opt-in, like the PostgreSQL and Docker tests: without their switch
// they skip, so `go test ./...` never needs a key or a network. They exist
// because every other test in this package answers an adapter from an
// httptest.Server shaped after a vendor's documentation, which exercises this
// code and proves nothing about a vendor's current wire format. Each test here
// makes exactly one real call, so one run costs one request.
//
// Nothing runs them in CI: a hosted-provider call needs a secret, and
// .github/AGENTS.md keeps those out of every workflow. Passing locally says the
// adapter and the vendor agreed on the day it ran, and nothing about any other
// day or model.
const (
	liveOpenAISwitch      = "TWINWRIGHT_TEST_LIVE_OPENAI"
	liveAnthropicSwitch   = "TWINWRIGHT_TEST_LIVE_ANTHROPIC"
	liveOpenAIModelEnv    = "TW_LIVE_OPENAI_MODEL"
	liveAnthropicModelEnv = "TW_LIVE_ANTHROPIC_MODEL"
)

// liveSettings reads a live test's key and model from getenv. When the test must
// not run it returns a non-empty skip message that names everything that is not
// set. A switch that is on while a variable is missing skips instead of failing:
// the operator asked for the run, and the message says what to add.
func liveSettings(getenv func(string) string, switchEnv, keyEnv, modelEnv string) (key, model, skip string) {
	if getenv(switchEnv) != "1" {
		return "", "", switchEnv + " is not 1; skipping the live provider call"
	}
	key, model = getenv(keyEnv), getenv(modelEnv)
	var missing []string
	if key == "" {
		missing = append(missing, keyEnv)
	}
	if model == "" {
		missing = append(missing, modelEnv)
	}
	if len(missing) > 0 {
		verb := "is"
		if len(missing) > 1 {
			verb = "are"
		}
		return "", "", switchEnv + "=1 but " + strings.Join(missing, " and ") + " " + verb + " not set; skipping the live provider call"
	}
	return key, model, ""
}

func liveConfig(t *testing.T, switchEnv, keyEnv, modelEnv string) (key, model string) {
	t.Helper()
	key, model, skip := liveSettings(os.Getenv, switchEnv, keyEnv, modelEnv)
	if skip != "" {
		t.Skip(skip)
	}
	return key, model
}

// liveOperations is the one tool a live call offers: getCustomer from the
// billing example, with its schema as the compiler produces it.
func liveOperations() []compiler.Operation {
	return []compiler.Operation{{ID: "getCustomer", Required: []string{"id"}, Properties: map[string]string{"id": "string"}}}
}

const liveTask = "Look up customer C-104 with the getCustomer tool. Call the tool now; do not answer in text."

// liveToolCallProblem says what is wrong with the turn a live call returned, or
// returns "" when it is well formed. The key is only compared against, never put
// into the text returned, so a failure cannot print it.
func liveToolCallProblem(m Message, key string) string {
	switch {
	case m.Role != "assistant":
		return "role is " + m.Role + ", want assistant"
	case m.ResponseID == "":
		return "the turn carries no response ID"
	case m.HTTPStatus != http.StatusOK:
		return "the turn records HTTP status " + strconv.Itoa(m.HTTPStatus) + ", want 200"
	case len(m.ToolCalls) == 0:
		return "the model made no tool call"
	}
	for _, call := range m.ToolCalls {
		if call.OperationID != "getCustomer" {
			return "the model called " + call.OperationID + ", which was not offered"
		}
		if call.ID == "" {
			return "a tool call has no ID"
		}
		if id, _ := call.Arguments["id"].(string); id == "" {
			return "the getCustomer call has no id argument"
		}
	}
	// The success path masks the key in every output item before it is
	// interpreted, so a model that echoed the key back must not leave it in the
	// turn that would be written to a ledger.
	encoded, err := json.Marshal(m)
	if err != nil {
		return "the turn does not marshal: " + err.Error()
	}
	if key != "" && strings.Contains(string(encoded), key) {
		return "the configured key appears in the turn"
	}
	return ""
}

func TestLiveOpenAIToolCall(t *testing.T) {
	key, model := liveConfig(t, liveOpenAISwitch, "OPENAI_API_KEY", liveOpenAIModelEnv)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p := OpenAIProvider{APIKey: key, Model: model, Timeout: 90 * time.Second}
	m, err := p.Next(ctx, liveTask, nil, liveOperations())
	if err != nil {
		t.Fatalf("OpenAI call failed: %v", err)
	}
	t.Logf("response_id=%s server_model=%s finish_reason=%s tool_calls=%d", m.ResponseID, m.ServerModel, m.FinishReason, len(m.ToolCalls))
	if problem := liveToolCallProblem(m, key); problem != "" {
		t.Fatalf("OpenAI turn: %s: %+v", problem, m)
	}
}

func TestLiveAnthropicToolCall(t *testing.T) {
	key, model := liveConfig(t, liveAnthropicSwitch, "ANTHROPIC_API_KEY", liveAnthropicModelEnv)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p := AnthropicProvider{APIKey: key, Model: model, Timeout: 90 * time.Second}
	m, err := p.Next(ctx, liveTask, nil, liveOperations())
	if err != nil {
		t.Fatalf("Anthropic call failed: %v", err)
	}
	t.Logf("response_id=%s server_model=%s finish_reason=%s tool_calls=%d", m.ResponseID, m.ServerModel, m.FinishReason, len(m.ToolCalls))
	if problem := liveToolCallProblem(m, key); problem != "" {
		t.Fatalf("Anthropic turn: %s: %+v", problem, m)
	}
}

// The live tests cannot run without a key, so what they assert is checked
// against the real adapters on turns this package builds itself: each adapter's
// documented tool-call shape must pass, and each way a turn can be malformed
// must not.
func TestLiveToolCallProblemJudgesTurnsFromTheAdapters(t *testing.T) {
	const key = "test-key-not-a-secret"
	serve := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(body)) }))
	}
	openAI := func(body string) Message {
		server := serve(body)
		defer server.Close()
		m, err := OpenAIProvider{APIKey: key, Model: "m", URL: server.URL, Client: server.Client()}.Next(context.Background(), liveTask, nil, liveOperations())
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	anthropic := func(body string) Message {
		server := serve(body)
		defer server.Close()
		m, err := AnthropicProvider{APIKey: key, Model: "m", URL: server.URL, Client: server.Client()}.Next(context.Background(), liveTask, nil, liveOperations())
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	goodCall := []ToolCall{{ID: "c", OperationID: "getCustomer", Arguments: map[string]any{"id": "C-104"}}}
	cases := []struct {
		name string
		turn Message
		want string // substring of the problem; "" means the turn is fine
	}{
		{"openai tool call", openAI(`{"id":"resp_1","model":"m-2026","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"getCustomer","arguments":"{\"id\":\"C-104\"}"}]}`), ""},
		{"anthropic tool call", anthropic(`{"id":"msg_1","model":"m-2026","stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"getCustomer","input":{"id":"C-104"}}]}`), ""},
		{"no response id", openAI(`{"output":[{"type":"function_call","call_id":"call_1","name":"getCustomer","arguments":"{\"id\":\"C-104\"}"}]}`), "no response ID"},
		{"text answer", openAI(`{"id":"resp_1","output":[{"type":"message","content":[{"type":"output_text","text":"Customer C-104 is Ada."}]}]}`), "no tool call"},
		{"a tool that was not offered", openAI(`{"id":"resp_1","output":[{"type":"function_call","call_id":"call_1","name":"deleteCustomer","arguments":"{\"id\":\"C-104\"}"}]}`), "not offered"},
		{"no call id", anthropic(`{"id":"msg_1","content":[{"type":"tool_use","id":"","name":"getCustomer","input":{"id":"C-104"}}]}`), "no ID"},
		{"no id argument", openAI(`{"id":"resp_1","output":[{"type":"function_call","call_id":"call_1","name":"getCustomer","arguments":"{}"}]}`), "no id argument"},
		{"a turn that is not the assistant's", Message{Role: "tool", ResponseID: "r", HTTPStatus: 200, ToolCalls: goodCall}, "want assistant"},
		{"an HTTP status that is not 200", Message{Role: "assistant", ResponseID: "r", HTTPStatus: 201, ToolCalls: goodCall}, "want 200"},
		{"the key echoed into the turn", Message{Role: "assistant", ResponseID: "r", HTTPStatus: 200, ToolCalls: []ToolCall{{ID: "c", OperationID: "getCustomer", Arguments: map[string]any{"id": key}}}}, "key appears"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := liveToolCallProblem(tc.turn, key)
			if tc.want == "" && got != "" {
				t.Fatalf("a well-formed turn was rejected: %s", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("problem = %q, want one containing %q", got, tc.want)
			}
			if strings.Contains(got, key) {
				t.Fatalf("the problem text carries the key: %q", got)
			}
		})
	}
}

// A live test must skip, and say what is missing, whenever it cannot run.
func TestLiveSettingsSkipAndNameWhatIsMissing(t *testing.T) {
	env := func(pairs ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i < len(pairs); i += 2 {
			m[pairs[i]] = pairs[i+1]
		}
		return func(k string) string { return m[k] }
	}
	cases := []struct {
		name      string
		getenv    func(string) string
		wantSkip  []string // every substring the skip message must carry; nil means no skip
		wantKey   string
		wantModel string
	}{
		{"nothing set", env(), []string{"TWINWRIGHT_TEST_LIVE_OPENAI is not 1"}, "", ""},
		{"key and model without the switch", env("OPENAI_API_KEY", "k", "TW_LIVE_OPENAI_MODEL", "m"), []string{"TWINWRIGHT_TEST_LIVE_OPENAI is not 1"}, "", ""},
		{"switch other than 1", env("TWINWRIGHT_TEST_LIVE_OPENAI", "true", "OPENAI_API_KEY", "k", "TW_LIVE_OPENAI_MODEL", "m"), []string{"is not 1"}, "", ""},
		{"switch without key or model", env("TWINWRIGHT_TEST_LIVE_OPENAI", "1"), []string{"OPENAI_API_KEY", "TW_LIVE_OPENAI_MODEL"}, "", ""},
		{"switch and model without key", env("TWINWRIGHT_TEST_LIVE_OPENAI", "1", "TW_LIVE_OPENAI_MODEL", "m"), []string{"OPENAI_API_KEY"}, "", ""},
		{"switch and key without model", env("TWINWRIGHT_TEST_LIVE_OPENAI", "1", "OPENAI_API_KEY", "k"), []string{"TW_LIVE_OPENAI_MODEL"}, "", ""},
		{"everything set", env("TWINWRIGHT_TEST_LIVE_OPENAI", "1", "OPENAI_API_KEY", "k", "TW_LIVE_OPENAI_MODEL", "m"), nil, "k", "m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, model, skip := liveSettings(tc.getenv, liveOpenAISwitch, "OPENAI_API_KEY", liveOpenAIModelEnv)
			if tc.wantSkip == nil && skip != "" {
				t.Fatalf("skipped with everything set: %s", skip)
			}
			for _, want := range tc.wantSkip {
				if !strings.Contains(skip, want) {
					t.Errorf("skip message %q does not mention %q", skip, want)
				}
			}
			if tc.wantSkip != nil && (key != "" || model != "") {
				t.Errorf("a skipped test was handed a key or model: %q %q", key, model)
			}
			if key != tc.wantKey || model != tc.wantModel {
				t.Errorf("key=%q model=%q, want %q and %q", key, model, tc.wantKey, tc.wantModel)
			}
		})
	}
}
