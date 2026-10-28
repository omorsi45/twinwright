package agent

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// A base URL that url.Parse refuses never reaches the transport, so the error
// that names it is the one http.NewRequestWithContext builds, and the standard
// library quotes the whole string in it: userinfo, query and all. The adapters
// used to redact only what client.Do returned. The runner writes the text of
// whatever the adapter returns into the ledger, the CLI prints it, and the trace
// exporter copies it into a span, so one unredacted error is a credential in
// every place a run is shared.
func TestMalformedBaseURLNeverLeaks(t *testing.T) {
	const query, userinfo = "query-key-token", "userinfo-pw-token"
	cases := []struct {
		name, url string
		leaks     []string
	}{
		{"bad escape in the password", "http://user:pa%ss-" + userinfo + "@127.0.0.1:1/v1?key=" + query, []string{"pa%ss", userinfo, query}},
		{"trailing newline", "http://user:" + userinfo + "@127.0.0.1:1/v1?key=" + query + "\n", []string{userinfo, query}},
		{"leading space", " http://user:" + userinfo + "@127.0.0.1:1/v1?key=" + query, []string{userinfo, query}},
		{"bad port", "http://user:" + userinfo + "@127.0.0.1:notaport/v1?key=" + query, []string{userinfo, query}},
		{"slash in the password", "http://user:" + userinfo + "/more@127.0.0.1:1/v1?key=" + query, []string{userinfo, query, "/more"}},
		{"hash in the password", "http://user:" + userinfo + "#more@127.0.0.1:1/v1?key=" + query, []string{userinfo, query, "more"}},
	}
	for _, adapter := range adapterCases() {
		for _, tc := range cases {
			t.Run(adapter.name+"/"+tc.name, func(t *testing.T) {
				out := executeWith(t, adapter.build(tc.url, 0))
				if out.err == nil || out.run.Status != "failed" {
					t.Fatalf("an unusable URL must fail the run: err=%v status=%s", out.err, out.run.Status)
				}
				// A configuration mistake is not something a second attempt fixes.
				var interrupted *CallInterrupted
				if errors.As(out.err, &interrupted) {
					t.Fatalf("a malformed URL was classified as an interruption: %v", out.err)
				}
				wantWaits(t, out.waits)
				wantInterruptions(t, recordedInterruptions(t, out.s, out.run.ID))

				events, err := out.s.Events(context.Background(), out.run.ID)
				if err != nil {
					t.Fatal(err)
				}
				everything := out.err.Error() + "\n" + out.run.Transcript
				for _, event := range events {
					everything += "\n" + string(event.Payload)
				}
				for _, secret := range append(tc.leaks, secretKey) {
					if strings.Contains(everything, secret) {
						t.Errorf("%q reached the error, transcript or ledger:\n%s", secret, everything)
					}
				}
				if !strings.Contains(out.err.Error(), "invalid URL") {
					t.Errorf("the error does not say the URL is the problem: %v", out.err)
				}
			})
		}
	}
}

// A request that fails to build is reported through the same redaction as one
// that fails to send, so the text is the same shape either way.
func TestRequestConstructionErrorKeepsOnlyTheAddress(t *testing.T) {
	for _, adapter := range adapterCases() {
		t.Run(adapter.name, func(t *testing.T) {
			_, err := adapter.build("http://user:pw-secret-token@127.0.0.1:1/v1?key=abc\n", 0).Next(context.Background(), "task", nil, metadataOps)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, leaked := range []string{"pw-secret-token", "abc", "user:"} {
				if strings.Contains(err.Error(), leaked) {
					t.Errorf("the error leaked %q: %v", leaked, err)
				}
			}
			if !strings.Contains(err.Error(), "127.0.0.1:1") {
				t.Errorf("the error lost the host it was about: %v", err)
			}
		})
	}
}

// The query of a base URL is part of the address, not of the path. Appending
// "/chat/completions" to the whole string put it inside the query, so a server
// that names an API version or an account there was sent to the wrong path.
func TestChatBaseURLQueryIsKept(t *testing.T) {
	for _, base := range []string{"/v1?api-version=2024-02-01", "/v1/?api-version=2024-02-01", "?api-version=2024-02-01"} {
		t.Run(base, func(t *testing.T) {
			var path, query string
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				path, query = r.URL.Path, r.URL.RawQuery
				w.Write([]byte(strings.ReplaceAll(adapterCases()[1].success, "ECHO", "ok")))
			})
			if _, err := adapterCases()[1].build(server.URL+base, 0).Next(context.Background(), "task", nil, metadataOps); err != nil {
				t.Fatal(err)
			}
			wantPath := "/chat/completions"
			if strings.HasPrefix(base, "/v1") {
				wantPath = "/v1/chat/completions"
			}
			if path != wantPath || query != "api-version=2024-02-01" {
				t.Fatalf("the server saw path %q and query %q, want path %q and the query unchanged", path, query, wantPath)
			}
		})
	}
}

// The cassette records the address through redact.URL, so a base URL's query is
// not in it, and a replay matches on the path alone. The query used to be glued to
// the path in the cassette's URL, before the suffix was joined to the path.
func TestCassetteOfAQueryBaseURLRecordsThePathAndReplays(t *testing.T) {
	const query = "api-version=2024-02-01&token=cassette-query-not-a-secret"
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.ReplaceAll(adapterCases()[1].success, "ECHO", "ok")))
	})
	path := filepath.Join(t.TempDir(), "query.cassette")
	rec, err := NewRecordingTransport(http.DefaultTransport, path, "openai-compatible", "m", secretKey)
	if err != nil {
		t.Fatal(err)
	}
	live := ChatCompletionsProvider{APIKey: secretKey, Model: "m", BaseURL: server.URL + "/v1?" + query, Client: &http.Client{Transport: rec}}
	liveTurn, err := live.Next(context.Background(), "task", nil, metadataOps)
	if err != nil {
		t.Fatal(err)
	}
	if err = rec.Close(); err != nil {
		t.Fatal(err)
	}
	c := loadCassette(t, path)
	recorded := c.Exchanges[0].URL
	if want := server.URL + "/v1/chat/completions"; recorded != want {
		t.Fatalf("recorded url = %q, want the path without the query, %q", recorded, want)
	}
	// What the CLI does to rebuild the adapter: cut the suffix off the first URL.
	base, found := strings.CutSuffix(recorded, "/chat/completions")
	if !found {
		t.Fatalf("the recorded url %q is not a chat completions endpoint", recorded)
	}
	replay := ChatCompletionsProvider{APIKey: "cassette-replay-placeholder", Model: "m", BaseURL: base, Client: &http.Client{Transport: c.Transport()}}
	turn, err := replay.Next(context.Background(), "task", nil, metadataOps)
	if err != nil {
		t.Fatalf("a cassette recorded against a base URL with a query does not replay: %v", err)
	}
	if turn.Content != liveTurn.Content {
		t.Fatalf("replay content = %q, want %q", turn.Content, liveTurn.Content)
	}
}
