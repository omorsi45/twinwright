package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"twinwright/internal/compiler"
)

// Every provider used to build its own http.Client with a flat 90 second
// timeout. Nothing could configure it, nothing could cancel it, and a hung
// provider call took the whole run down with it 90 seconds later.
//
// The deadline is now per attempt and carried on the context, so a hang is
// cancelled when the caller said it should be and comes back as something the
// runner can recognise rather than as an opaque transport error.
func TestProviderCallIsCancelledAtItsOwnDeadline(t *testing.T) {
	ops := []compiler.Operation{{ID: "getCharge", Required: []string{"id"}, Properties: map[string]string{"id": "string"}}}
	// A server that accepts the request and never answers. The handler is
	// released before the server is closed: cleanups run last-registered-first,
	// and closing a server whose handlers are still blocked waits forever.
	hang := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(hang) })

	deadline := 150 * time.Millisecond
	for _, c := range []struct {
		name     string
		provider Provider
	}{
		{"openai", OpenAIProvider{APIKey: "k", Model: "m", URL: server.URL, Client: server.Client(), Timeout: deadline}},
		{"anthropic", AnthropicProvider{APIKey: "k", Model: "m", URL: server.URL, Client: server.Client(), Timeout: deadline}},
		{"chat completions", ChatCompletionsProvider{Model: "m", BaseURL: server.URL + "/v1", Client: server.Client(), Timeout: deadline}},
	} {
		t.Run(c.name, func(t *testing.T) {
			started := time.Now()
			_, err := c.provider.Next(context.Background(), "Inspect", nil, ops)
			elapsed := time.Since(started)
			var interrupted *CallInterrupted
			if !errors.As(err, &interrupted) {
				t.Fatalf("a hung call must report an interruption the runner can act on, got %v", err)
			}
			if interrupted.Kind != InterruptTimeout || interrupted.Deadline != deadline {
				t.Fatalf("%+v", interrupted)
			}
			// Generous: the point is that it is nowhere near the old flat 90s.
			if elapsed > 5*time.Second {
				t.Fatalf("call took %s, so it was not cancelled at its deadline", elapsed)
			}
		})
	}
}

// A cancelled caller is the run going away, not the provider hanging. Reporting
// it as a provider timeout would have the runner retry a call nobody is waiting
// for any more.
func TestCancelledCallerIsNotReportedAsAProviderTimeout(t *testing.T) {
	hang := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hang
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(hang) })
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := OpenAIProvider{APIKey: "k", Model: "m", URL: server.URL, Client: server.Client(), Timeout: time.Minute}.Next(ctx, "Inspect", nil, nil)
	var interrupted *CallInterrupted
	if errors.As(err, &interrupted) {
		t.Fatalf("caller cancellation reported as a provider interruption: %+v", interrupted)
	}
	if err == nil {
		t.Fatal("a cancelled call must still fail")
	}
}

// A completion the provider cut short at its token ceiling arrives as HTTP 200
// with a well-formed body. The only signal is a metadata field, and it is spelled
// differently on each surface: stop_reason on Anthropic's Messages API,
// finish_reason on an OpenAI-compatible chat completion, and status plus
// incomplete_details.reason on OpenAI's Responses API.
//
// Consuming the tool call inside such a body is the failure this guards: the
// arguments were cut mid-serialisation, so the agent would act on a fragment that
// happens to parse.
func TestTruncatedCompletionIsNotTreatedAsADecision(t *testing.T) {
	ops := []compiler.Operation{{ID: "getCharge", Required: []string{"id"}, Properties: map[string]string{"id": "string"}}}
	for _, c := range []struct {
		name     string
		body     string
		reason   string
		provider func(url string, client *http.Client) Provider
	}{
		{
			name:   "openai responses status incomplete",
			body:   `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"function_call","call_id":"call-1","name":"getCharge","arguments":"{\"id\":\"CH-100"}]}`,
			reason: "max_output_tokens",
			provider: func(url string, client *http.Client) Provider {
				return OpenAIProvider{APIKey: "k", Model: "m", URL: url, Client: client}
			},
		},
		{
			name:   "anthropic stop_reason max_tokens",
			body:   `{"stop_reason":"max_tokens","content":[{"type":"tool_use","id":"toolu_1","name":"getCharge","input":{"id":"CH-100"}}]}`,
			reason: "max_tokens",
			provider: func(url string, client *http.Client) Provider {
				return AnthropicProvider{APIKey: "k", Model: "m", URL: url, Client: client}
			},
		},
		{
			name:   "chat completions finish_reason length",
			body:   `{"choices":[{"finish_reason":"length","message":{"content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"getCharge","arguments":"{\"id\":\"CH-100\"}"}}]}}]}`,
			reason: "length",
			provider: func(url string, client *http.Client) Provider {
				return ChatCompletionsProvider{Model: "m", BaseURL: url + "/v1", Client: client}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(c.body))
			}))
			defer server.Close()
			message, err := c.provider(server.URL, server.Client()).Next(context.Background(), "Inspect", nil, ops)
			var interrupted *CallInterrupted
			if !errors.As(err, &interrupted) {
				t.Fatalf("a truncated completion must not come back as a usable turn: message=%+v err=%v", message, err)
			}
			if interrupted.Kind != InterruptTruncated || interrupted.Reason != c.reason {
				t.Fatalf("%+v", interrupted)
			}
			if len(message.ToolCalls) != 0 {
				t.Fatalf("a tool call was parsed out of a truncated completion: %+v", message.ToolCalls)
			}
		})
	}
}

// The complement, so the guard cannot pass by rejecting everything: a completion
// the provider finished normally still yields its tool call, on every surface,
// including the values that mean "stopped because it called a tool".
func TestCompletedResponseWithAStopReasonStillYieldsItsToolCall(t *testing.T) {
	ops := []compiler.Operation{{ID: "getCharge", Required: []string{"id"}, Properties: map[string]string{"id": "string"}}}
	for _, c := range []struct {
		name     string
		body     string
		provider func(url string, client *http.Client) Provider
	}{
		{
			name: "openai responses status completed",
			body: `{"status":"completed","output":[{"type":"function_call","call_id":"call-1","name":"getCharge","arguments":"{\"id\":\"CH-1002\"}"}]}`,
			provider: func(url string, client *http.Client) Provider {
				return OpenAIProvider{APIKey: "k", Model: "m", URL: url, Client: client}
			},
		},
		{
			name: "anthropic stop_reason tool_use",
			body: `{"stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_1","name":"getCharge","input":{"id":"CH-1002"}}]}`,
			provider: func(url string, client *http.Client) Provider {
				return AnthropicProvider{APIKey: "k", Model: "m", URL: url, Client: client}
			},
		},
		{
			name: "chat completions finish_reason tool_calls",
			body: `{"choices":[{"finish_reason":"tool_calls","message":{"content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"getCharge","arguments":"{\"id\":\"CH-1002\"}"}}]}}]}`,
			provider: func(url string, client *http.Client) Provider {
				return ChatCompletionsProvider{Model: "m", BaseURL: url + "/v1", Client: client}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(c.body))
			}))
			defer server.Close()
			message, err := c.provider(server.URL, server.Client()).Next(context.Background(), "Inspect", nil, ops)
			if err != nil {
				t.Fatalf("a completed response was rejected: %v", err)
			}
			if len(message.ToolCalls) != 1 || message.ToolCalls[0].OperationID != "getCharge" {
				t.Fatalf("%+v", message)
			}
		})
	}
}
