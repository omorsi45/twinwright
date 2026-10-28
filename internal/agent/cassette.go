package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"

	"twinwright/internal/redact"
)

// CassetteVersion is the cassette file format this build writes and reads.
const CassetteVersion = 1

// ErrCassetteDivergence is wrapped by the error a cassette transport returns when
// the request it is asked to serve is not the one that was recorded next.
var ErrCassetteDivergence = errors.New("cassette divergence")

// CassetteHeader is the first line of a cassette file.
type CassetteHeader struct {
	Version  int    `json:"twinwright_cassette"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// MaxOutputTokens is the output ceiling the adapter was built with. It is
	// part of the request body, so a replay has to rebuild the adapter with the
	// same value or every request hashes differently. Omitted when no ceiling was
	// configured. Older readers ignore it, which is why it did not need a new
	// version.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
}

// Exchange is one HTTP round trip as it crossed the wire, one JSON line of a
// cassette file.
//
// What a cassette cannot reproduce, stated once because replay's guarantee is
// only as wide as this list:
//
//   - A deadline. A per-attempt timeout is recorded as a failed round trip (the
//     error text is all there is of it), and replays as a transport
//     interruption, so the replayed ledger differs from the recorded one and the
//     replay reports a divergence. Deadlines are not simulated.
//   - A Retry-After given as an HTTP date. The wait is worked out against the
//     clock when the response arrives, so a replay at another time can compute a
//     different wait. Only the header text is recorded.
//   - A response body that is not valid UTF-8. It is stored as a JSON string.
//
// Request headers are never recorded, so a credential sent in one cannot reach
// the file. The URL loses its userinfo and query, and both bodies are masked with
// the secrets the recorder was given. RequestSHA256 is the one field computed
// before masking: it is how a replay matches a request without the key.
type Exchange struct {
	// Seq is 1-based and counts every exchange in the file, failed round trips
	// included.
	Seq    int    `json:"seq"`
	Method string `json:"method"`
	// URL has had redact.URL applied: no userinfo, no query. The secrets are
	// masked in what is left, since some gateways take the key in the path.
	URL string `json:"url"`
	// RequestSHA256 is of the request body bytes as sent.
	RequestSHA256 string `json:"request_sha256"`
	// Request is the request body with redact.Secrets applied. It is the body
	// itself when that is JSON, and a JSON string holding it otherwise.
	Request    json.RawMessage `json:"request"`
	Status     int             `json:"status"`
	RetryAfter string          `json:"retry_after,omitempty"`
	// ContentType is the response's Content-Type header, one of the two response
	// headers a cassette replays.
	ContentType string `json:"content_type,omitempty"`
	// Response is the response body with redact.Secrets applied.
	Response string `json:"response"`
	// Error is set when the round trip did not complete. With Status 0 there was
	// no response at all and replay returns this as an error. With a Status the
	// body was cut short, and replay serves what was read and then this error.
	// The text has had redact.String and redact.URL applied.
	Error string `json:"error,omitempty"`
}

// RecordingTransport is an http.RoundTripper that passes each request to a base
// transport and appends the exchange to a cassette file before handing the
// response back.
//
// It is safe for concurrent use. Recording cannot be allowed to fail quietly: a
// cassette missing an exchange replays as a divergence nobody can explain. So a
// write that fails fails the round trip, and every later one, instead of letting
// the run carry on against a file that is no longer a record of it.
type RecordingTransport struct {
	base    http.RoundTripper
	secrets []string

	mu     sync.Mutex
	file   *os.File
	seq    int
	failed error
	closed bool
}

var errCassetteClosed = errors.New("cassette recording is closed")

// NewRecordingTransport creates (or truncates) the cassette at path, writes its
// header, and returns a transport that records through base. The secrets are
// masked wherever they appear in a recorded body; the API key is the usual one.
func NewRecordingTransport(base http.RoundTripper, path, provider, model string, secrets ...string) (*RecordingTransport, error) {
	return NewRecordingTransportWith(base, path, CassetteHeader{Provider: provider, Model: model}, secrets...)
}

// NewRecordingTransportWith is NewRecordingTransport for a caller that also has
// an output ceiling to put in the header. The version is always the current one.
func NewRecordingTransportWith(base http.RoundTripper, path string, header CassetteHeader, secrets ...string) (*RecordingTransport, error) {
	if base == nil {
		base = http.DefaultTransport
	}
	header.Version = CassetteVersion
	line, err := encodeLine(header)
	if err != nil {
		return nil, err
	}
	// 0600: the bodies are the prompts and answers of a run, even masked.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create cassette: %w", err)
	}
	if _, err = file.Write(line); err != nil {
		file.Close()
		return nil, fmt.Errorf("write cassette %s: %w", path, err)
	}
	return &RecordingTransport{base: base, secrets: secrets, file: file}, nil
}

// encodeLine is one JSON line. HTML escaping is off so a body keeps the
// characters it was sent with.
func encodeLine(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sha256Sum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// RoundTrip sends the request through the base transport and records the
// exchange. The caller sees what the base transport returned: the same status,
// headers and body bytes, or the same error.
func (t *RecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.usable(); err != nil {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, err
	}
	// The body is read to hash it and handed on as a copy. A RoundTripper may not
	// change the request it is given, so the base transport gets a clone.
	forward := req
	var body []byte
	if req.Body != nil && req.Body != http.NoBody {
		var err error
		body, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		forward = req.Clone(req.Context())
		forward.Body = io.NopCloser(bytes.NewReader(body))
		forward.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	ex := Exchange{
		Method:        req.Method,
		URL:           redact.Secrets(redact.URL(req.URL.String()), t.secrets...),
		RequestSHA256: sha256Sum(body),
		Request:       t.requestJSON(body),
	}

	res, err := t.base.RoundTrip(forward)
	if err != nil {
		ex.Error = t.errorText(err)
		if writeErr := t.record(ex); writeErr != nil {
			return nil, fmt.Errorf("%w; recording it also failed: %v", err, writeErr)
		}
		return nil, err
	}

	// One byte past the adapters' limit, no more: they refuse a longer body as
	// too large, and reading it all here would let a hostile server fill the disk.
	data, readErr := io.ReadAll(io.LimitReader(res.Body, MaxResponseBytes+1))
	res.Body.Close()
	ex.Status = res.StatusCode
	ex.ContentType = res.Header.Get("Content-Type")
	ex.RetryAfter = res.Header.Get("Retry-After")
	ex.Response = redact.Secrets(string(data), t.secrets...)
	if readErr != nil {
		ex.Error = t.errorText(readErr)
	}
	if writeErr := t.record(ex); writeErr != nil {
		return nil, writeErr
	}
	served := *res
	if readErr != nil {
		served.Body = &failingBody{data: data, err: readErr}
	} else {
		served.Body = io.NopCloser(bytes.NewReader(data))
	}
	return &served, nil
}

// requestJSON is the recorded form of a request body: the masked body when that
// is JSON, and the masked text as a JSON string when it is not.
func (t *RecordingTransport) requestJSON(body []byte) json.RawMessage {
	masked := redact.Secrets(string(body), t.secrets...)
	if json.Valid([]byte(masked)) {
		return json.RawMessage(masked)
	}
	encoded, _ := json.Marshal(masked)
	return encoded
}

var urlInText = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)

// errorText is an error made safe to write down: every URL in it loses its
// userinfo and query, then the secrets and anything that looks like a key are
// masked. A transport error routinely quotes the URL it was dialling.
func (t *RecordingTransport) errorText(err error) string {
	return redact.String(urlInText.ReplaceAllStringFunc(err.Error(), redact.URL), t.secrets...)
}

func (t *RecordingTransport) usable() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failed != nil {
		return t.failed
	}
	if t.closed {
		return errCassetteClosed
	}
	return nil
}

// record appends one exchange. The seq is taken here, under the lock, so file
// order and seq order cannot differ however many round trips are in flight.
func (t *RecordingTransport) record(ex Exchange) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failed != nil {
		return t.failed
	}
	if t.closed {
		return errCassetteClosed
	}
	ex.Seq = t.seq + 1
	line, err := encodeLine(ex)
	if err == nil {
		// One Write per exchange, straight to the file: it is in the operating
		// system's hands before the response is, so a crash loses nothing the
		// caller was told about.
		_, err = t.file.Write(line)
	}
	if err != nil {
		t.failed = fmt.Errorf("write cassette %s: %w", t.file.Name(), err)
		return t.failed
	}
	t.seq++
	return nil
}

// Close ends the recording. It is safe to call more than once.
func (t *RecordingTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	t.closed = true
	return t.file.Close()
}

// failingBody is a response body that yields what was read and then the error
// that stopped the read.
type failingBody struct {
	data []byte
	err  error
}

func (b *failingBody) Read(p []byte) (int, error) {
	if len(b.data) > 0 {
		n := copy(p, b.data)
		b.data = b.data[n:]
		return n, nil
	}
	return 0, b.err
}

func (b *failingBody) Close() error { return nil }

// Cassette is a loaded cassette file.
type Cassette struct {
	Header    CassetteHeader
	Exchanges []Exchange
}

// LoadCassette reads and validates a cassette file. It refuses a version it does
// not know, a header without a provider or model, a seq that is not 1, 2, 3 in
// order, and a status that is neither a response nor a recorded failure, so a
// damaged or truncated file is reported where it is read and not as a puzzling
// divergence later.
func LoadCassette(path string) (*Cassette, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open cassette: %w", err)
	}
	defer file.Close()
	dec := json.NewDecoder(file)
	var c Cassette
	if err = dec.Decode(&c.Header); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("cassette %s: no header line", path)
		}
		return nil, fmt.Errorf("cassette %s: header: %w", path, err)
	}
	if c.Header.Version != CassetteVersion {
		return nil, fmt.Errorf("cassette %s: unsupported cassette version %d (this build reads version %d)", path, c.Header.Version, CassetteVersion)
	}
	if c.Header.Provider == "" || c.Header.Model == "" {
		return nil, fmt.Errorf("cassette %s: the header names no provider or model", path)
	}
	for n := 1; ; n++ {
		var ex Exchange
		if err = dec.Decode(&ex); err != nil {
			if errors.Is(err, io.EOF) {
				return &c, nil
			}
			return nil, fmt.Errorf("cassette %s: exchange %d: %w", path, n, err)
		}
		switch {
		case ex.Seq != n:
			return nil, fmt.Errorf("cassette %s: exchange %d: seq %d, want %d", path, n, ex.Seq, n)
		case ex.Method == "" || ex.URL == "":
			return nil, fmt.Errorf("cassette %s: exchange %d: no method or url", path, n)
		case ex.Status == 0 && ex.Error == "":
			return nil, fmt.Errorf("cassette %s: exchange %d: status 0 without an error", path, n)
		case ex.Status != 0 && (ex.Status < 100 || ex.Status > 599):
			return nil, fmt.Errorf("cassette %s: exchange %d: status %d is not an HTTP status", path, n, ex.Status)
		}
		c.Exchanges = append(c.Exchanges, ex)
	}
}

// CheckRun refuses a cassette recorded for a different provider or model than
// the run it is about to answer: its responses are answers to questions that run
// never asked.
func (c *Cassette) CheckRun(provider, model string) error {
	if c.Header.Provider == provider && c.Header.Model == model {
		return nil
	}
	return fmt.Errorf("cassette was recorded for %s/%s, but the run used %s/%s", c.Header.Provider, c.Header.Model, provider, model)
}

// Transport serves the recorded exchanges in order and never dials: it has no
// base transport and touches no network. Each call returns a new transport that
// starts again at the first exchange.
//
// A request is matched to the next exchange by method, then URL path, then
// request_sha256, and the first field that differs is named in the error with the
// seq. The host and the query are not compared, so a cassette recorded against
// one address replays against another. The cursor does not move on a divergence.
// A recorded failure replays as an error and never as a response.
func (c *Cassette) Transport() http.RoundTripper {
	paths := make([]string, len(c.Exchanges))
	for i, ex := range c.Exchanges {
		paths[i] = ex.URL
		if u, err := url.Parse(ex.URL); err == nil {
			paths[i] = u.Path
		}
	}
	return &cassetteTransport{exchanges: c.Exchanges, paths: paths}
}

type cassetteTransport struct {
	exchanges []Exchange
	paths     []string

	mu   sync.Mutex
	next int
}

func (t *cassetteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	seq := t.next + 1
	if t.next >= len(t.exchanges) {
		return nil, fmt.Errorf("%w: seq %d: no recorded exchange left (the cassette holds %d)", ErrCassetteDivergence, seq, len(t.exchanges))
	}
	ex := t.exchanges[t.next]
	switch {
	case req.Method != ex.Method:
		return nil, fmt.Errorf("%w: seq %d: method differs", ErrCassetteDivergence, seq)
	case req.URL.Path != t.paths[t.next]:
		return nil, fmt.Errorf("%w: seq %d: url path differs", ErrCassetteDivergence, seq)
	case sha256Sum(body) != ex.RequestSHA256:
		return nil, fmt.Errorf("%w: seq %d: request_sha256 differs", ErrCassetteDivergence, seq)
	}
	t.next++
	if ex.Status == 0 {
		return nil, errors.New(ex.Error)
	}
	header := http.Header{}
	if ex.ContentType != "" {
		header.Set("Content-Type", ex.ContentType)
	}
	if ex.RetryAfter != "" {
		header.Set("Retry-After", ex.RetryAfter)
	}
	res := &http.Response{
		Status:        fmt.Sprintf("%d %s", ex.Status, http.StatusText(ex.Status)),
		StatusCode:    ex.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(ex.Response)),
		ContentLength: int64(len(ex.Response)),
		Request:       req,
	}
	if ex.Error != "" {
		res.Body = &failingBody{data: []byte(ex.Response), err: errors.New(ex.Error)}
		res.ContentLength = -1
	}
	return res, nil
}
