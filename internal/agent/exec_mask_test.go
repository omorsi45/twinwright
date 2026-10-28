package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"twinwright/internal/agent/exectest"
	"twinwright/internal/dispatch"
	"twinwright/internal/store"
)

// replyProvider is an agent that answers every turn with the given bytes.
func replyProvider(t testing.TB, reply string, extra ...string) ExecProvider {
	t.Helper()
	file := filepath.Join(t.TempDir(), "reply.json")
	if err := os.WriteFile(file, []byte(reply), 0o600); err != nil {
		t.Fatal(err)
	}
	return helperProvider(t, "reply", append([]string{exectest.ReplyFile + "=" + file}, extra...)...)
}

const replyHead = `{"protocol":"twinwright.agent.v1",`

// Masking works on the decoded response, never on its bytes. A passed value that
// happens to be the digits of a number, or part of them, is not a string in the
// response, and rewriting the bytes made a valid document invalid and blamed the
// agent for it.
func TestExecMaskingKeepsTheResponseValid(t *testing.T) {
	reply := replyHead + `"content":"","tool_calls":[{"id":"a","operation_id":"getCustomer","arguments":{"n":123456789,"ok":true,"none":null}}]}`
	message, err := replyProvider(t, reply, "TW_ACCOUNT=12345678").Next(context.Background(), "task", nil, nil)
	if err != nil {
		t.Fatalf("a valid response was refused: %v", err)
	}
	if len(message.ToolCalls) != 1 {
		t.Fatalf("message = %+v", message)
	}
	want := map[string]any{"n": json.Number("123456789"), "ok": true, "none": nil}
	if got := message.ToolCalls[0].Arguments; !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments = %#v, want %#v: only strings are masked", got, want)
	}
}

// Every string of the decoded response is masked: the content, and in each call
// the id, the operation, and the names and values of the arguments at any depth.
func TestExecMasksEveryStringOfTheDecodedResponse(t *testing.T) {
	const passed = "tenant-0042"
	document := map[string]any{
		"protocol": AgentProtocol,
		"content":  "account " + passed,
		"tool_calls": []any{map[string]any{
			"id":           "call-" + passed,
			"operation_id": "getCustomer",
			"arguments": map[string]any{
				"id":     passed,
				"nested": map[string]any{"list": []any{"x " + passed, 7}, passed: 1},
			},
		}},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	message, err := replyProvider(t, string(raw), "TW_TENANT="+passed).Next(context.Background(), "task", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "account [REDACTED]" {
		t.Errorf("content = %q", message.Content)
	}
	call := message.ToolCalls[0]
	if call.ID != "call-[REDACTED]" || call.OperationID != "getCustomer" {
		t.Errorf("call = %+v", call)
	}
	want := map[string]any{
		"id":     "[REDACTED]",
		"nested": map[string]any{"list": []any{"x [REDACTED]", json.Number("7")}, "[REDACTED]": json.Number("1")},
	}
	if !reflect.DeepEqual(call.Arguments, want) {
		t.Errorf("arguments = %#v, want %#v", call.Arguments, want)
	}
}

// What is masked is the value the response decodes to, so how the agent spelled
// it on the wire does not matter: \uXXXX for non-ASCII, which is what Python's
// json.dumps writes, a quote escaped, or a slash escaped.
func TestExecMasksValuesHoweverTheAgentEncodedThem(t *testing.T) {
	cases := []struct{ name, value, wire string }{
		{"non-ASCII as \\u escapes", "p\u00e4ssw\u00f6rd-value-1", `key=p\u00e4ssw\u00f6rd-value-1`},
		{"ampersand and escaped quote", `a&b"c-value-1234`, `key=a&b\"c-value-1234`},
		{"escaped slash", `a/b-value-1234`, `key=a\/b-value-1234`},
		{"Go style markup escapes", `a&b<c>d-value-1234`, `key=a\u0026b\u003cc\u003ed-value-1234`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply := replyHead + `"content":"` + tc.wire + `","tool_calls":[]}`
			message, err := replyProvider(t, reply, "TW_PASSED="+tc.value).Next(context.Background(), "task", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if message.Content != "key=[REDACTED]" {
				t.Fatalf("content = %q, want the value masked", message.Content)
			}
		})
	}
}

// The reference agent is written in Python and encodes with json.dumps defaults,
// which is the encoder the masking has to cope with.
func TestExecMasksWhatPythonEncodes(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	script := `import json, os, sys; sys.stdin.read(); print(json.dumps({"protocol": "twinwright.agent.v1", "content": "key=" + os.environ["TW_PASSED"], "tool_calls": []}))`
	for _, value := range []string{"p\u00e4ssw\u00f6rd-value-1", `a&b"c-value-1234`, "line-one\nline-two-value"} {
		p := ExecProvider{Command: []string{python, "-c", script}, Env: []string{"TW_PASSED=" + value}, Timeout: 30 * time.Second}
		message, err := p.Next(context.Background(), "task", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if message.Content != "key=[REDACTED]" {
			t.Errorf("value %q: content = %q, want it masked", value, message.Content)
		}
	}
}

// An agent that uses a value it was given as an argument has the value masked
// before the call is dispatched, so the ledger and the tool alike see the mask:
// the call is recorded, and runs, with "[REDACTED]" where the value was. That
// changes what the tool receives, and it is the only way the tool does not
// receive the value.
func TestExecMaskedArgumentsAreWhatTheLedgerAndTheToolSee(t *testing.T) {
	const passed = "tenant-0042"
	ctx := context.Background()
	manifest := execBillingManifest(t)
	s, err := store.Open(filepath.Join(t.TempDir(), "world.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	world, err := s.Seed(ctx, 42, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, world.ID, "duplicate-charge", "exec", "echo@test", "task", "listCharges")
	if err != nil {
		t.Fatal(err)
	}
	provider := helperProvider(t, "call-with-secret", exectest.Secret+"="+passed)
	runner := Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: provider}
	done, err := runner.Execute(ctx, run.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "completed" {
		t.Fatalf("status = %s", done.Status)
	}

	var arguments string
	var status int
	if err := s.DB.QueryRow("SELECT arguments, status FROM tool_results WHERE run_id=? AND call_id='exec-1'", run.ID).Scan(&arguments, &status); err != nil {
		t.Fatal(err)
	}
	if arguments != `{"id":"[REDACTED]"}` || status != 404 {
		t.Errorf("the tool received %s and answered %d, want the masked value and a 404 for a customer that does not exist", arguments, status)
	}
	if strings.Contains(done.Transcript, passed) {
		t.Errorf("the transcript holds the passed value: %.300s", done.Transcript)
	}
	events, err := s.Events(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(string(event.Payload), passed) {
			t.Errorf("ledger event %s (%s) holds the passed value: %.300s", event.ID, event.Type, event.Payload)
		}
	}
}
