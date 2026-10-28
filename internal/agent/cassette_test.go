package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"twinwright/internal/compiler"
)

// roundTripFunc lets a test stand in for the network or for a failing one.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var cassetteOps = []compiler.Operation{{ID: "getCharge", Required: []string{"id"}, Properties: map[string]string{"id": "string"}}}

// toolReply is the tool message that follows the first scripted turn.
var toolReply = Message{Role: "tool", CallID: "call-1", Content: `{"id":"CH-1002"}`}

// chatRecording is one two-call conversation recorded against a scripted server
// that has since been closed.
type chatRecording struct {
	path          string
	base          string   // the BaseURL the adapter was given
	endpoint      string   // the URL the adapter called
	bodies        [][]byte // request bodies the server received, in order
	first, second Message
}

// recordChat drives the tool round trip of TestChatCompletionsToolRoundTrip
// through a recording transport: a tool call, then a final answer. The server
// echoes the Authorization header back into the answer the way a careless proxy
// might, so every recording also exercises the masking of a response body.
func recordChat(t *testing.T, key, task string) chatRecording {
	t.Helper()
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(string(body), `"role":"tool"`) {
			w.Write([]byte(`{"id":"chatcmpl-1","model":"local-server-1","choices":[{"message":{"content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"getCharge","arguments":"{\"id\":\"CH-1002\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
			return
		}
		fmt.Fprintf(w, `{"id":"chatcmpl-2","model":"local-server-1","choices":[{"message":{"content":"Done %s"},"finish_reason":"stop"}]}`, r.Header.Get("Authorization"))
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "run.cassette")
	rec, err := NewRecordingTransport(http.DefaultTransport, path, "openai-compatible", "local", key)
	if err != nil {
		t.Fatal(err)
	}
	base := server.URL + "/v1"
	p := ChatCompletionsProvider{APIKey: key, Model: "local", BaseURL: base, Client: &http.Client{Transport: rec}}
	first, err := p.Next(context.Background(), task, nil, cassetteOps)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Next(context.Background(), task, []Message{first, toolReply}, cassetteOps)
	if err != nil {
		t.Fatal(err)
	}
	if err = rec.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return chatRecording{path: path, base: base, endpoint: base + "/chat/completions", bodies: bodies, first: first, second: second}
}

func loadCassette(t *testing.T, path string) *Cassette {
	t.Helper()
	c, err := LoadCassette(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// replayChat is the adapter a replay builds: the same provider and model, the
// recorded address, a placeholder key, and the cassette as its only transport.
func replayChat(c *Cassette, base string) ChatCompletionsProvider {
	return ChatCompletionsProvider{APIKey: "cassette-replay-placeholder", Model: "local", BaseURL: base, Client: &http.Client{Transport: c.Transport()}}
}

func post(rt http.RoundTripper, method, target string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	return rt.RoundTrip(req)
}

func TestRecordingTransportWritesJSONLines(t *testing.T) {
	rec := recordChat(t, secretKey, "Inspect")
	data, err := os.ReadFile(rec.path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("the file does not end with a newline: %q", data[max(len(data)-20, 0):])
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines, want a header and two exchanges:\n%s", len(lines), data)
	}
	if want := `{"twinwright_cassette":1,"provider":"openai-compatible","model":"local"}`; lines[0] != want {
		t.Fatalf("header = %s, want %s", lines[0], want)
	}
	for i, line := range lines[1:] {
		var ex Exchange
		if err := json.Unmarshal([]byte(line), &ex); err != nil {
			t.Fatalf("exchange %d is not JSON: %v", i+1, err)
		}
		if ex.Seq != i+1 || ex.Method != http.MethodPost || ex.URL != rec.endpoint || ex.Status != http.StatusOK || ex.ContentType != "application/json" {
			t.Fatalf("exchange %d = %+v", i+1, ex)
		}
		if ex.RequestSHA256 != sha256Hex(rec.bodies[i]) {
			t.Fatalf("exchange %d request_sha256 = %s, want the hash of the bytes the server received", i+1, ex.RequestSHA256)
		}
		if !equalJSON(ex.Request, rec.bodies[i]) {
			t.Fatalf("exchange %d request = %s, want %s", i+1, ex.Request, rec.bodies[i])
		}
		if ex.Error != "" || ex.RetryAfter != "" {
			t.Fatalf("exchange %d carries an error or Retry-After it never had: %+v", i+1, ex)
		}
	}
}

// The recorded answers go back through the adapter that asked for them, so what
// comes out is the adapter's own parse of the recorded bytes and nothing else.
func TestCassetteReplaysTheRecordedTurns(t *testing.T) {
	rec := recordChat(t, secretKey, "Inspect")
	p := replayChat(loadCassette(t, rec.path), rec.base)
	first, err := p.Next(context.Background(), "Inspect", nil, cassetteOps)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, rec.first) {
		t.Fatalf("first turn\n got %+v\nwant %+v", first, rec.first)
	}
	second, err := p.Next(context.Background(), "Inspect", []Message{first, toolReply}, cassetteOps)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second, rec.second) {
		t.Fatalf("second turn\n got %+v\nwant %+v", second, rec.second)
	}
}

// The scripted server is closed before the cassette is read, and every other
// route to the network is wired to fail the test, so a turn that comes back can
// only have come from the file.
func TestCassetteNeverDials(t *testing.T) {
	rec := recordChat(t, secretKey, "Inspect")
	c := loadCassette(t, rec.path)

	var dials atomic.Int32
	saved := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		dials.Add(1)
		return nil, errors.New("the network was used")
	})
	t.Cleanup(func() { http.DefaultTransport = saved })

	p := replayChat(c, rec.base)
	first, err := p.Next(context.Background(), "Inspect", nil, cassetteOps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Next(context.Background(), "Inspect", []Message{first, toolReply}, cassetteOps); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 0 {
		t.Fatalf("the cassette reached the network %d times", dials.Load())
	}
	// The tripwire itself works: a client that falls back to the default
	// transport is caught by it.
	if _, err = (&http.Client{}).Get(rec.base); err == nil || dials.Load() != 1 {
		t.Fatalf("the default transport was not replaced: err=%v dials=%d", err, dials.Load())
	}
}

func TestCassetteDivergenceNamesTheExchange(t *testing.T) {
	rec := recordChat(t, secretKey, "Inspect")
	c := loadCassette(t, rec.path)

	// Through the adapter: the history changed between the two turns, so the
	// second request is not the one that was recorded.
	p := replayChat(c, rec.base)
	first, err := p.Next(context.Background(), "Inspect", nil, cassetteOps)
	if err != nil {
		t.Fatal(err)
	}
	altered := Message{Role: "tool", CallID: "call-1", Content: `{"id":"CH-9999"}`}
	_, err = p.Next(context.Background(), "Inspect", []Message{first, altered}, cassetteOps)
	if !errors.Is(err, ErrCassetteDivergence) {
		t.Fatalf("an altered history was served from the cassette: %v", err)
	}
	if !strings.Contains(err.Error(), "seq 2") || !strings.Contains(err.Error(), "request_sha256") {
		t.Fatalf("the error does not name the exchange and the field: %v", err)
	}
	var interrupt *CallInterrupted
	if !errors.As(err, &interrupt) || interrupt.Kind != InterruptTransport {
		t.Fatalf("the adapter reports a divergence as a transport failure, got %T %v", err, err)
	}

	// Straight through the transport, one field at a time, in the order the
	// fields are checked.
	body := rec.bodies[0]
	otherBody := []byte(`{"altered":true}`)
	cases := []struct {
		name           string
		consume        int // exchanges served correctly first
		method, target string
		body           []byte
		want           string
	}{
		{"request body", 0, http.MethodPost, rec.endpoint, otherBody, "cassette divergence: seq 1: request_sha256 differs"},
		{"request body after one exchange", 1, http.MethodPost, rec.endpoint, otherBody, "cassette divergence: seq 2: request_sha256 differs"},
		{"url path", 0, http.MethodPost, rec.base + "/other", body, "cassette divergence: seq 1: url path differs"},
		{"method", 0, http.MethodGet, rec.endpoint, body, "cassette divergence: seq 1: method differs"},
		{"method before path and body", 0, http.MethodGet, rec.base + "/other", otherBody, "cassette divergence: seq 1: method differs"},
		{"path before body", 0, http.MethodPost, rec.base + "/other", otherBody, "cassette divergence: seq 1: url path differs"},
		{"exhausted", 2, http.MethodPost, rec.endpoint, body, "cassette divergence: seq 3:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := c.Transport()
			for i := 0; i < tc.consume; i++ {
				res, err := post(rt, http.MethodPost, rec.endpoint, rec.bodies[i])
				if err != nil {
					t.Fatal(err)
				}
				res.Body.Close()
			}
			res, err := post(rt, tc.method, tc.target, tc.body)
			if res != nil {
				res.Body.Close()
				t.Fatalf("a diverging request was answered with HTTP %d", res.StatusCode)
			}
			if !errors.Is(err, ErrCassetteDivergence) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one wrapping ErrCassetteDivergence that says %q", err, tc.want)
			}
		})
	}

	// The host and the query are deliberately not compared: a cassette recorded
	// against one address replays against another.
	res, err := post(c.Transport(), http.MethodPost, "http://elsewhere.example:9/v1/chat/completions?x=1", body)
	if err != nil {
		t.Fatalf("a different host and query diverged: %v", err)
	}
	res.Body.Close()
}

func TestCassetteHoldsNoSecrets(t *testing.T) {
	// The key is in the task as well, so it reaches the request body.
	rec := recordChat(t, secretKey, "Look it up with the key "+secretKey)
	data, err := os.ReadFile(rec.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{secretKey, "Bearer " + secretKey, "Authorization", "authorization", "x-api-key"} {
		if bytes.Contains(data, []byte(leak)) {
			t.Errorf("the cassette contains %q", leak)
		}
	}
	if !bytes.Contains(data, []byte("[REDACTED]")) {
		t.Errorf("the key was in the request and the response but nothing was masked:\n%s", data)
	}
	// The hash is over the bytes as sent, so it is not the hash of the masked
	// text. That is what lets a replay with a placeholder key match.
	var ex Exchange
	line := strings.Split(string(data), "\n")[1]
	if err = json.Unmarshal([]byte(line), &ex); err != nil {
		t.Fatal(err)
	}
	if ex.RequestSHA256 != sha256Hex(rec.bodies[0]) || !bytes.Contains(rec.bodies[0], []byte(secretKey)) {
		t.Errorf("request_sha256 %s is not over the unmasked bytes as sent", ex.RequestSHA256)
	}

	// Replaying with a placeholder key serves the same answers: the masked
	// bodies are what the live run also stored.
	p := replayChat(loadCassette(t, rec.path), rec.base)
	first, err := p.Next(context.Background(), "Look it up with the key "+secretKey, nil, cassetteOps)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Next(context.Background(), "Look it up with the key "+secretKey, []Message{first, toolReply}, cassetteOps)
	if err != nil || !reflect.DeepEqual(second, rec.second) {
		t.Fatalf("second=%+v err=%v, want %+v", second, err, rec.second)
	}
}

// Everything a round trip can carry besides the body: request headers, the
// userinfo and query of the URL, and response headers other than the two kept.
func TestRecordingTransportLeavesCredentialsOutOfTheFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Set-Cookie", "session="+secretKey)
		w.Header().Set("X-Echo-Key", secretKey)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"echo":%q}`, secretKey)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "run.cassette")
	rec, err := NewRecordingTransport(http.DefaultTransport, path, "openai", "m", secretKey)
	if err != nil {
		t.Fatal(err)
	}
	target := strings.Replace(server.URL, "http://", "http://operator:userinfo-not-real@", 1) + "/v1/responses?key=" + secretKey
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(`{"input":"`+secretKey+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("x-api-key", secretKey)
	req.Header.Set("Cookie", "k="+secretKey)
	res, err := rec.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(io.Discard, res.Body); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if err = rec.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{secretKey, "userinfo-not-real", "operator", "key=", "session=", "X-Echo-Key", "Cookie"} {
		if bytes.Contains(data, []byte(leak)) {
			t.Errorf("the cassette contains %q:\n%s", leak, data)
		}
	}
	c := loadCassette(t, path)
	if want := server.URL + "/v1/responses"; c.Exchanges[0].URL != want {
		t.Errorf("recorded url = %q, want %q", c.Exchanges[0].URL, want)
	}
}

// A round trip with no response is still an exchange. It takes a seq, so the
// attempt that followed it lines up, and it replays as an error and never as a
// response.
func TestRecordingKeepsAFailedRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Write([]byte(`{"id":"chatcmpl-1","model":"m","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	var calls atomic.Int32
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			// What a dialer or a proxy might say: a full URL with userinfo and a
			// key in the query, and the key again in the text.
			dirty := strings.Replace(req.URL.String(), "http://", "http://operator:userinfo-not-real@", 1) + "?key=" + secretKey
			return nil, &url.Error{Op: "Post", URL: dirty, Err: errors.New("connection reset by peer, token " + secretKey)}
		}
		return http.DefaultTransport.RoundTrip(req)
	})
	path := filepath.Join(t.TempDir(), "run.cassette")
	rec, err := NewRecordingTransport(base, path, "openai-compatible", "local", secretKey)
	if err != nil {
		t.Fatal(err)
	}
	live := ChatCompletionsProvider{APIKey: secretKey, Model: "local", BaseURL: server.URL + "/v1", Client: &http.Client{Transport: rec}}
	var interrupt *CallInterrupted
	if _, err = live.Next(context.Background(), "task", nil, cassetteOps); !errors.As(err, &interrupt) || interrupt.Kind != InterruptTransport {
		t.Fatalf("the live failure was not a transport interruption: %v", err)
	}
	liveTurn, err := live.Next(context.Background(), "task", nil, cassetteOps)
	if err != nil {
		t.Fatal(err)
	}
	if err = rec.Close(); err != nil {
		t.Fatal(err)
	}
	server.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{secretKey, "userinfo-not-real", "operator", "key="} {
		if bytes.Contains(data, []byte(leak)) {
			t.Errorf("the cassette contains %q:\n%s", leak, data)
		}
	}
	c := loadCassette(t, path)
	if len(c.Exchanges) != 2 {
		t.Fatalf("%d exchanges, want the failed one and the one that landed", len(c.Exchanges))
	}
	failed, landed := c.Exchanges[0], c.Exchanges[1]
	if failed.Seq != 1 || failed.Status != 0 || failed.Response != "" || !strings.Contains(failed.Error, "connection reset by peer") {
		t.Fatalf("failed exchange = %+v", failed)
	}
	if failed.Method != http.MethodPost || failed.URL != server.URL+"/v1/chat/completions" || failed.RequestSHA256 != sha256Hex(bodies[0]) {
		t.Fatalf("the failed exchange does not describe the request: %+v", failed)
	}
	if landed.Seq != 2 || landed.Status != http.StatusOK {
		t.Fatalf("landed exchange = %+v", landed)
	}

	// Replay: the first attempt errors, the second is the recorded answer.
	p := replayChat(c, server.URL+"/v1")
	if _, err = p.Next(context.Background(), "task", nil, cassetteOps); !errors.As(err, &interrupt) || interrupt.Kind != InterruptTransport {
		t.Fatalf("the recorded failure did not replay as a transport interruption: %v", err)
	}
	turn, err := p.Next(context.Background(), "task", nil, cassetteOps)
	if err != nil || !reflect.DeepEqual(turn, liveTurn) {
		t.Fatalf("turn=%+v err=%v, want %+v", turn, err, liveTurn)
	}
}

func TestCassetteServesOnlyTheRecordedStatusBodyAndTwoHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Retry-After", "7")
		w.Header().Set("Set-Cookie", "a=b")
		w.Header().Set("X-Request-Id", "req-1")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "run.cassette")
	rec, err := NewRecordingTransport(http.DefaultTransport, path, "openai", "m")
	if err != nil {
		t.Fatal(err)
	}
	res, err := post(rec, http.MethodPost, server.URL+"/v1/responses", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	rec.Close()
	server.Close()

	c := loadCassette(t, path)
	if ex := c.Exchanges[0]; ex.Status != 429 || ex.RetryAfter != "7" || ex.ContentType != "application/json; charset=utf-8" {
		t.Fatalf("exchange = %+v", ex)
	}
	served, err := post(c.Transport(), http.MethodPost, server.URL+"/v1/responses", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer served.Body.Close()
	var names []string
	for name := range served.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"Content-Type", "Retry-After"}) {
		t.Fatalf("served headers %v, want only Content-Type and Retry-After", names)
	}
	body, _ := io.ReadAll(served.Body)
	if served.StatusCode != 429 || served.Header.Get("Retry-After") != "7" || string(body) != `{"error":{"message":"slow down"}}` {
		t.Fatalf("served %d %v %s", served.StatusCode, served.Header, body)
	}
}

// A body that dies part way is the adapter's problem to classify, so the cassette
// hands it the same partial bytes and the same error.
func TestCassetteKeepsAResponseCutShort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Length", "100")
		w.Write([]byte(`{"partial":`))
	}))
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "run.cassette")
	rec, err := NewRecordingTransport(http.DefaultTransport, path, "openai", "m")
	if err != nil {
		t.Fatal(err)
	}
	res, err := post(rec, http.MethodPost, server.URL+"/v1/responses", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	liveBody, liveErr := io.ReadAll(res.Body)
	res.Body.Close()
	rec.Close()
	server.Close()
	if liveErr == nil || string(liveBody) != `{"partial":` {
		t.Fatalf("the recording changed what the caller saw: %q %v", liveBody, liveErr)
	}

	served, err := post(loadCassette(t, path).Transport(), http.MethodPost, server.URL+"/v1/responses", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer served.Body.Close()
	body, err := io.ReadAll(served.Body)
	if err == nil || err.Error() != liveErr.Error() || string(body) != `{"partial":` {
		t.Fatalf("replayed body %q err %v, want %q err %v", body, err, liveBody, liveErr)
	}
}

// The recorder reads no more than the adapter would. A longer body is cut at the
// same byte the adapter's limit is checked at, so the replay is refused the same
// way and the file does not grow without bound.
func TestRecordingStopsAtTheResponseLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write(bytes.Repeat([]byte("x"), MaxResponseBytes+10))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "run.cassette")
	rec, err := NewRecordingTransport(http.DefaultTransport, path, "openai", "m")
	if err != nil {
		t.Fatal(err)
	}
	res, err := post(rec, http.MethodPost, server.URL+"/v1/responses", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	live, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || len(live) != MaxResponseBytes+1 {
		t.Fatalf("the caller saw %d bytes, err %v, want %d", len(live), err, MaxResponseBytes+1)
	}
	rec.Close()
	if got := loadCassette(t, path).Exchanges[0].Response; len(got) != MaxResponseBytes+1 {
		t.Fatalf("recorded %d bytes, want %d", len(got), MaxResponseBytes+1)
	}
}

func TestRecordedRequestThatIsNotJSONIsStoredAsAString(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "run.cassette")
	rec, err := NewRecordingTransport(http.DefaultTransport, path, "openai", "m", secretKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"plain text " + secretKey, ""} {
		res, err := post(rec, http.MethodPost, server.URL+"/v1/responses", []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	rec.Close()
	c := loadCassette(t, path)
	if got := string(c.Exchanges[0].Request); got != `"plain text [REDACTED]"` {
		t.Errorf("request = %s", got)
	}
	if got := string(c.Exchanges[1].Request); got != `""` {
		t.Errorf("empty request = %s", got)
	}
	if c.Exchanges[1].RequestSHA256 != sha256Hex(nil) {
		t.Errorf("empty request hash = %s", c.Exchanges[1].RequestSHA256)
	}
}

func TestCassetteHeaderCarriesTheOutputCeiling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.cassette")
	rec, err := NewRecordingTransportWith(http.DefaultTransport, path, CassetteHeader{Provider: "anthropic", Model: "claude-test", MaxOutputTokens: 512})
	if err != nil {
		t.Fatal(err)
	}
	rec.Close()
	data, _ := os.ReadFile(path)
	if want := `{"twinwright_cassette":1,"provider":"anthropic","model":"claude-test","max_output_tokens":512}` + "\n"; string(data) != want {
		t.Fatalf("header = %q, want %q", data, want)
	}
	c := loadCassette(t, path)
	if c.Header.MaxOutputTokens != 512 || c.Header.Provider != "anthropic" || c.Header.Model != "claude-test" || len(c.Exchanges) != 0 {
		t.Fatalf("header = %+v with %d exchanges", c.Header, len(c.Exchanges))
	}
}

func TestLoadCassetteRefusesWhatItCannotTrust(t *testing.T) {
	header := `{"twinwright_cassette":1,"provider":"p","model":"m"}` + "\n"
	exchange := func(seq int, status int, extra string) string {
		return fmt.Sprintf(`{"seq":%d,"method":"POST","url":"http://h/p","request_sha256":"00","request":{},"status":%d,"response":""%s}`+"\n", seq, status, extra)
	}
	cases := []struct{ name, content, want string }{
		{"empty file", "", "no header"},
		{"header is not JSON", "not json\n", "header"},
		{"later version", `{"twinwright_cassette":2,"provider":"p","model":"m"}` + "\n", "version 2"},
		{"not a cassette", `{"provider":"p","model":"m"}` + "\n", "version 0"},
		{"header without provider", `{"twinwright_cassette":1,"model":"m"}` + "\n", "provider"},
		{"header without model", `{"twinwright_cassette":1,"provider":"p"}` + "\n", "model"},
		{"exchange is not JSON", header + "garbage\n", "exchange 1"},
		{"truncated exchange", header + `{"seq":1,"method":"POST"`, "exchange 1"},
		{"seq starts at 2", header + exchange(2, 200, ""), "exchange 1: seq 2"},
		{"seq skips", header + exchange(1, 200, "") + exchange(3, 200, ""), "exchange 2: seq 3"},
		{"status 0 without an error", header + exchange(1, 0, ""), "exchange 1"},
		{"status out of range", header + exchange(1, 700, ""), "exchange 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "run.cassette")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadCassette(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadCassette error = %v, want one that says %q", err, tc.want)
			}
		})
	}
	if _, err := LoadCassette(filepath.Join(t.TempDir(), "absent.cassette")); err == nil {
		t.Fatal("a missing file loaded")
	}
	// A failed round trip is a valid exchange.
	path := filepath.Join(t.TempDir(), "ok.cassette")
	if err := os.WriteFile(path, []byte(header+exchange(1, 0, `,"error":"connection refused"`)+exchange(2, 200, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := LoadCassette(path); err != nil || len(c.Exchanges) != 2 {
		t.Fatalf("c=%+v err=%v", c, err)
	}
}

// The cassette was recorded for one provider and model. Serving it to a run that
// used another would replay answers to questions that run never asked.
func TestCassetteCheckRunNamesBothSides(t *testing.T) {
	c := &Cassette{Header: CassetteHeader{Version: CassetteVersion, Provider: "openai", Model: "gpt-recorded"}}
	if err := c.CheckRun("openai", "gpt-recorded"); err != nil {
		t.Fatal(err)
	}
	for _, run := range [][2]string{{"anthropic", "gpt-recorded"}, {"openai", "gpt-other"}} {
		err := c.CheckRun(run[0], run[1])
		if err == nil {
			t.Fatalf("a cassette for openai/gpt-recorded was accepted for %s/%s", run[0], run[1])
		}
		for _, want := range []string{"openai/gpt-recorded", run[0] + "/" + run[1]} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	}
}

func TestRecordingTransportIsSafeForConcurrentUse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "run.cassette")
	rec, err := NewRecordingTransport(http.DefaultTransport, path, "openai", "m")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := post(rec, http.MethodPost, server.URL+"/v1/responses", []byte(`{}`))
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}()
	}
	wg.Wait()
	if err = rec.Close(); err != nil {
		t.Fatal(err)
	}
	// LoadCassette refuses a seq gap or a torn line, so loading is the check.
	if c := loadCassette(t, path); len(c.Exchanges) != 8 {
		t.Fatalf("%d exchanges, want 8", len(c.Exchanges))
	}
	if _, err = post(rec, http.MethodPost, server.URL+"/v1/responses", []byte(`{}`)); err == nil {
		t.Fatal("a closed recording transport still made a request")
	}
}

func TestNewRecordingTransportRefusesAnUnwritablePath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent-directory", "run.cassette")
	if _, err := NewRecordingTransport(http.DefaultTransport, missing, "openai", "m"); err == nil {
		t.Fatal("a path in a directory that does not exist was accepted")
	}
}
