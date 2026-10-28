package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The success-path mask used to cover only a model's output items. A proxy that
// echoes the request back can put the key in the response's own id, model or
// stop reason as well, and those went to the turn and the ledger as sent.
func TestEchoedTopLevelFieldsAreMasked(t *testing.T) {
	for _, tc := range adapterCases() {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.ReplaceAll(tc.success, "ECHO", "ok")
			for _, field := range []string{tc.want.ResponseID, tc.want.ServerModel, tc.want.FinishReason} {
				if !strings.Contains(body, `"`+field+`"`) {
					t.Fatalf("the fixture has no %q field to echo into", field)
				}
				body = strings.Replace(body, `"`+field+`"`, `"`+field+`-`+secretKey+`"`, 1)
			}
			server := serve(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
			got, err := tc.build(server.URL, 0).Next(context.Background(), "task", nil, metadataOps)
			if err != nil {
				t.Fatal(err)
			}
			for name, value := range map[string]string{"response_id": got.ResponseID, "server_model": got.ServerModel, "finish_reason": got.FinishReason} {
				if strings.Contains(value, secretKey) || !strings.Contains(value, "[REDACTED]") {
					t.Errorf("%s = %q, want the key masked", name, value)
				}
			}
		})
	}
}

// Some gateways take the key in the URL path. The address is printed in a
// transport error and written to a cassette, so it is masked like any other text.
func TestKeyInTheURLPathIsMasked(t *testing.T) {
	for _, tc := range adapterCases() {
		t.Run(tc.name+"/transport error", func(t *testing.T) {
			_, err := tc.build("http://127.0.0.1:1/"+secretKey+"/v1", 0).Next(context.Background(), "task", nil, metadataOps)
			if err == nil {
				t.Fatal("expected a transport error")
			}
			if strings.Contains(err.Error(), secretKey) {
				t.Fatalf("the error carries the key from the URL path: %v", err)
			}
		})
	}
	t.Run("cassette", func(t *testing.T) {
		server := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(strings.ReplaceAll(adapterCases()[1].success, "ECHO", "ok")))
		})
		path := filepath.Join(t.TempDir(), "run.cassette")
		rec, err := NewRecordingTransport(http.DefaultTransport, path, "openai-compatible", "m", secretKey)
		if err != nil {
			t.Fatal(err)
		}
		provider := ChatCompletionsProvider{APIKey: secretKey, Model: "m", BaseURL: server.URL + "/" + secretKey + "/v1", Client: &http.Client{Transport: rec}}
		if _, err = provider.Next(context.Background(), "task", nil, metadataOps); err != nil {
			t.Fatal(err)
		}
		if err = rec.Close(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(secretKey)) {
			t.Fatalf("the cassette carries the key from the URL path:\n%s", data)
		}
		if got := loadCassette(t, path).Exchanges[0].URL; !strings.HasSuffix(got, "/[REDACTED]/v1/chat/completions") {
			t.Fatalf("recorded url = %q, want the key masked in the path", got)
		}
	})
}

// The mask runs on the raw JSON text, where a key is spelled the way the server's
// encoder spelled it. A Go server escapes "&" and "<" as \u0026 and \u003c, and
// other encoders escape "/" as "\/". The key as written does not appear in
// either, so a mask that looked only for the raw key let the decoded content and
// tool arguments carry the key into the ledger.
func TestSuccessfulOutputIsRedactedWhenEscaped(t *testing.T) {
	const key = "gw-key/with&slashes-<123"
	encoded, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	spellings := map[string]string{
		"raw":             key,
		"encoder escaped": string(encoded[1 : len(encoded)-1]),
		"escaped slashes": strings.ReplaceAll(key, "/", `\/`),
		"all escaped":     strings.ReplaceAll(string(encoded[1:len(encoded)-1]), "/", `\/`),
	}
	if !strings.Contains(spellings["encoder escaped"], `\u0026`) {
		t.Fatalf("the fixture key is not escaped by the encoder: %s", spellings["encoder escaped"])
	}
	providers := map[string]func(url string) Provider{
		"openai": func(url string) Provider { return OpenAIProvider{APIKey: key, Model: "m", URL: url} },
		"openai-compatible": func(url string) Provider {
			return ChatCompletionsProvider{APIKey: key, Model: "m", BaseURL: url}
		},
		"anthropic": func(url string) Provider { return AnthropicProvider{APIKey: key, Model: "m", URL: url} },
	}
	for _, tc := range adapterCases() {
		for name, spelled := range spellings {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				server := serve(t, func(w http.ResponseWriter, r *http.Request) {
					w.Write([]byte(strings.ReplaceAll(tc.success, "ECHO", spelled)))
				})
				got, err := providers[tc.name](server.URL).Next(context.Background(), "task", nil, metadataOps)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(got.Content, key) || !strings.Contains(got.Content, "[REDACTED]") {
					t.Fatalf("content = %q, want the key masked", got.Content)
				}
				if len(got.RawOutput) == 0 {
					t.Fatal("no raw output to check")
				}
				for _, raw := range got.RawOutput {
					if !json.Valid(raw) {
						t.Fatalf("redaction broke the raw item: %s", raw)
					}
					var decoded any
					if err = json.Unmarshal(raw, &decoded); err != nil {
						t.Fatal(err)
					}
					// The bytes hold the spelling the server used and the decoded
					// strings hold the key itself; either one in the ledger is a leak.
					if strings.Contains(string(raw), spelled) || stringsContain(decoded, key) {
						t.Fatalf("raw output leaked the key (%s): %s", name, raw)
					}
				}
			})
		}
	}
}

// stringsContain reports whether any string inside a decoded JSON value holds needle.
func stringsContain(v any, needle string) bool {
	switch v := v.(type) {
	case string:
		return strings.Contains(v, needle)
	case []any:
		for _, item := range v {
			if stringsContain(item, needle) {
				return true
			}
		}
	case map[string]any:
		for _, item := range v {
			if stringsContain(item, needle) {
				return true
			}
		}
	}
	return false
}
