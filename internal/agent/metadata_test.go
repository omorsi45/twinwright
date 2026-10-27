package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"twinwright/internal/compiler"
)

// secretKey is long enough to be masked and assembled so a scanner does not
// mistake the fixture for a credential.
const secretKey = "fixture-" + "secret-key-123456"

// adapterCase describes one provider against a fake server: how to build it
// pointing at a URL, the success body it answers with (ECHO stands for text the
// model repeats back), and the request field that carries the output ceiling.
type adapterCase struct {
	name       string
	build      func(url string, max int) Provider
	success    string
	tokenField string
	// defaultTokens is what the adapter sends when no ceiling was configured.
	defaultTokens float64
	want          Message
}

func adapterCases() []adapterCase {
	return []adapterCase{
		{
			name: "openai",
			build: func(url string, max int) Provider {
				return OpenAIProvider{APIKey: secretKey, Model: "m", URL: url, MaxOutputTokens: max}
			},
			success:    `{"id":"resp_1","model":"gpt-server-1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Done ECHO"}]}]}`,
			tokenField: "max_output_tokens",
			want:       Message{ResponseID: "resp_1", ServerModel: "gpt-server-1", FinishReason: "completed", HTTPStatus: 200},
		},
		{
			name: "openai-compatible",
			build: func(url string, max int) Provider {
				return ChatCompletionsProvider{APIKey: secretKey, Model: "m", BaseURL: url, MaxOutputTokens: max}
			},
			success:    `{"id":"chatcmpl-1","model":"local-server-1","choices":[{"message":{"role":"assistant","content":"Done ECHO"},"finish_reason":"stop"}]}`,
			tokenField: "max_tokens",
			want:       Message{ResponseID: "chatcmpl-1", ServerModel: "local-server-1", FinishReason: "stop", HTTPStatus: 200},
		},
		{
			name: "anthropic",
			build: func(url string, max int) Provider {
				return AnthropicProvider{APIKey: secretKey, Model: "m", URL: url, MaxOutputTokens: max}
			},
			success:       `{"id":"msg_1","model":"claude-server-1","stop_reason":"end_turn","content":[{"type":"text","text":"Done ECHO"}]}`,
			tokenField:    "max_tokens",
			defaultTokens: DefaultMaxOutputTokens,
			want:          Message{ResponseID: "msg_1", ServerModel: "claude-server-1", FinishReason: "end_turn", HTTPStatus: 200},
		},
	}
}

var metadataOps = []compiler.Operation{{ID: "getCharge", Required: []string{"id"}, Properties: map[string]string{"id": "string"}}}

func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func TestAdaptersRecordResponseMetadata(t *testing.T) {
	for _, tc := range adapterCases() {
		t.Run(tc.name, func(t *testing.T) {
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(strings.ReplaceAll(tc.success, "ECHO", "ok")))
			})
			got, err := tc.build(server.URL, 0).Next(context.Background(), "task", nil, metadataOps)
			if err != nil {
				t.Fatal(err)
			}
			if got.ResponseID != tc.want.ResponseID || got.ServerModel != tc.want.ServerModel ||
				got.FinishReason != tc.want.FinishReason || got.HTTPStatus != tc.want.HTTPStatus {
				t.Fatalf("metadata = id %q model %q finish %q status %d, want %+v", got.ResponseID, got.ServerModel, got.FinishReason, got.HTTPStatus, tc.want)
			}
		})
	}
}

// A body that fills the read limit used to be cut silently and then fail to
// decode, or worse decode as a shorter answer. The size is its own error, and it
// is not an interruption, so the runner does not spend a second call on it.
func TestOversizedResponseIsAnExplicitError(t *testing.T) {
	for _, tc := range adapterCases() {
		t.Run(tc.name, func(t *testing.T) {
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(strings.Repeat("x", 5<<20)))
			})
			_, err := tc.build(server.URL, 0).Next(context.Background(), "task", nil, metadataOps)
			if !errors.Is(err, ErrResponseTooLarge) {
				t.Fatalf("err = %v, want ErrResponseTooLarge", err)
			}
			var interrupt *CallInterrupted
			if errors.As(err, &interrupt) {
				t.Fatalf("an oversized response must not be retried as an interruption: %v", err)
			}
		})
	}
}

func TestResponseAtTheLimitIsStillAccepted(t *testing.T) {
	// MaxResponseBytes exactly is allowed; only one byte more is refused.
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		body := `{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"` + "A"
		pad := MaxResponseBytes - len(body) - len(`"}]}]}`)
		w.Write([]byte(body + strings.Repeat("A", pad) + `"}]}]}`))
	})
	if _, err := (OpenAIProvider{APIKey: secretKey, Model: "m", URL: server.URL}).Next(context.Background(), "task", nil, metadataOps); err != nil {
		t.Fatalf("a body of exactly MaxResponseBytes was refused: %v", err)
	}
}

func TestSuccessfulOutputIsRedacted(t *testing.T) {
	for _, tc := range adapterCases() {
		t.Run(tc.name, func(t *testing.T) {
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(strings.ReplaceAll(tc.success, "ECHO", secretKey)))
			})
			got, err := tc.build(server.URL, 0).Next(context.Background(), "task", nil, metadataOps)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(got.Content, secretKey) || !strings.Contains(got.Content, "[REDACTED]") {
				t.Fatalf("content = %q", got.Content)
			}
			if len(got.RawOutput) == 0 {
				t.Fatal("no raw output to check")
			}
			for _, raw := range got.RawOutput {
				if strings.Contains(string(raw), secretKey) {
					t.Fatalf("raw output leaked the key: %s", raw)
				}
				if !json.Valid(raw) {
					t.Fatalf("redaction broke the raw item: %s", raw)
				}
			}
		})
	}
}

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

func TestTransportErrorHidesURLQueryAndUserinfo(t *testing.T) {
	for _, tc := range adapterCases() {
		t.Run(tc.name+"/dial", func(t *testing.T) {
			_, err := tc.build("http://user:pw@127.0.0.1:1/v1?key=abc", 0).Next(context.Background(), "task", nil, metadataOps)
			if err == nil {
				t.Fatal("expected a transport error")
			}
			if strings.Contains(err.Error(), "pw") || strings.Contains(err.Error(), "abc") {
				t.Fatalf("the error leaked the URL userinfo or query: %v", err)
			}
			if !strings.Contains(err.Error(), "127.0.0.1:1") {
				t.Fatalf("the error lost the host it was about: %v", err)
			}
		})
		t.Run(tc.name+"/key in error text", func(t *testing.T) {
			client := &http.Client{Transport: failingTransport{err: errors.New("proxy rejected " + secretKey)}}
			var p Provider
			switch built := tc.build("http://127.0.0.1:1/v1", 0).(type) {
			case OpenAIProvider:
				built.Client = client
				p = built
			case ChatCompletionsProvider:
				built.Client = client
				p = built
			case AnthropicProvider:
				built.Client = client
				p = built
			}
			_, err := p.Next(context.Background(), "task", nil, metadataOps)
			if err == nil || strings.Contains(err.Error(), secretKey) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestMaxOutputTokensReachesTheRequest(t *testing.T) {
	for _, tc := range adapterCases() {
		for _, configured := range []int{0, 321} {
			name := tc.name + "/default"
			want := tc.defaultTokens
			if configured > 0 {
				name = tc.name + "/configured"
				want = float64(configured)
			}
			t.Run(name, func(t *testing.T) {
				var sent map[string]any
				server := serve(t, func(w http.ResponseWriter, r *http.Request) {
					if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
						t.Errorf("request JSON: %v", err)
					}
					w.Write([]byte(strings.ReplaceAll(tc.success, "ECHO", "ok")))
				})
				if _, err := tc.build(server.URL, configured).Next(context.Background(), "task", nil, metadataOps); err != nil {
					t.Fatal(err)
				}
				got, present := sent[tc.tokenField]
				if want == 0 {
					if present {
						t.Fatalf("%s sent %v although no ceiling was configured", tc.tokenField, got)
					}
					return
				}
				if got != want {
					t.Fatalf("%s = %v, want %v", tc.tokenField, got, want)
				}
			})
		}
	}
}
