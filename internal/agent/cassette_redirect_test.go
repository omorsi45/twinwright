package agent

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The recorder sits under http.Client, so a 307 or 308 hop is recorded as its own
// exchange. The replay used to serve the 3xx without its Location header, and Go's
// client returns a redirect it cannot follow as the response, so the adapter failed
// with "chat completions HTTP 308" and the run could not be replayed.
func TestCassetteReplaysARedirectedExchange(t *testing.T) {
	const token = "redirect-token-not-a-secret"
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if r.URL.Path == "/old/chat/completions" {
					http.Redirect(w, r, "/v1/chat/completions?token="+token, status)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"c","model":"m","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer server.Close()

			path := filepath.Join(t.TempDir(), "redirect.cassette")
			rec, err := NewRecordingTransport(http.DefaultTransport, path, "openai-compatible", "m", secretKey)
			if err != nil {
				t.Fatal(err)
			}
			live := ChatCompletionsProvider{Model: "m", BaseURL: server.URL + "/old", Client: &http.Client{Transport: rec}}
			// A 301 or 302 re-sends the request as a GET with no body, which the
			// server's second handler answers the same way, so the shape of the
			// follow-up does not change what is recorded here.
			liveTurn, err := live.Next(context.Background(), "task", nil, cassetteOps)
			if err != nil {
				t.Fatalf("live: %v", err)
			}
			if err = rec.Close(); err != nil {
				t.Fatal(err)
			}

			c := loadCassette(t, path)
			if len(c.Exchanges) != 2 || c.Exchanges[0].Status != status || c.Exchanges[1].Status != http.StatusOK {
				t.Fatalf("exchanges = %+v, want the redirect and then the answer", c.Exchanges)
			}
			// The target is kept for the replay and loses its query, like any URL.
			if got := c.Exchanges[0].Location; got != "/v1/chat/completions" {
				t.Errorf("recorded location = %q, want the target without its query", got)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte(token)) {
				t.Errorf("the redirect target's query reached the cassette:\n%s", data)
			}

			replay := ChatCompletionsProvider{APIKey: "cassette-replay-placeholder", Model: "m", BaseURL: server.URL + "/old", Client: &http.Client{Transport: c.Transport()}}
			turn, err := replay.Next(context.Background(), "task", nil, cassetteOps)
			if err != nil {
				t.Fatalf("a recorded redirect does not replay: %v", err)
			}
			if turn.Content != liveTurn.Content || turn.ResponseID != liveTurn.ResponseID {
				t.Errorf("replay turn = %+v, want %+v", turn, liveTurn)
			}
		})
	}
}

// Only a redirect carries a Location the replay needs, and a header the response
// to a request that was not redirected happens to carry is not written down.
func TestCassetteRecordsNoLocationForAnOrdinaryResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Location", "http://elsewhere.example/?token=ordinary-token-not-a-secret")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "ordinary.cassette")
	rec, err := NewRecordingTransport(http.DefaultTransport, path, "openai", "m")
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := rec.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if err = rec.Close(); err != nil {
		t.Fatal(err)
	}
	if got := loadCassette(t, path).Exchanges[0].Location; got != "" {
		t.Fatalf("a 201 recorded location %q", got)
	}
}
