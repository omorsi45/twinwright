package trace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"twinwright/internal/redact"
)

// DefaultExportTimeout bounds one export attempt.
const DefaultExportTimeout = 10 * time.Second

// Exporter posts OTLP JSON to an OpenTelemetry collector over HTTP.
//
// This is deliberately a plain HTTP POST of the document OTLP already produces,
// rather than a dependency on the OpenTelemetry SDK. Twinwright's traces are
// derived from the durable ledger after the fact, not emitted live from
// instrumented code, so the SDK's span lifecycle, samplers, context propagation
// and batching processors would all be unused machinery. What matters here is
// that the bytes reach a real collector, which is what the endpoint and the
// status-code check establish.
//
// Nothing the exporter returns carries the endpoint's userinfo or query or a
// header value. The endpoint is passed through redact.URL wherever it is named,
// in Delivery.Endpoint and in every error, and header values are masked in error
// text. The endpoint of a hosted collector routinely holds a token, and both
// callers print what comes back.
type Exporter struct {
	// Endpoint is the collector's base URL, for example
	// http://127.0.0.1:4318. The OTLP traces path is appended to the URL's path
	// when absent, and a query string on the endpoint is kept.
	Endpoint string
	// Headers are sent with the request, for a collector behind an auth proxy.
	Headers map[string]string
	// Client defaults to an http.Client with Timeout set.
	Client *http.Client
	// Timeout applies when Client is nil.
	Timeout time.Duration
}

// tracesPath is the OTLP/HTTP path for traces.
const tracesPath = "/v1/traces"

// tracesURL appends the OTLP/HTTP traces path to the endpoint's path unless the
// caller already gave a full path, so both forms work. The query is kept as it
// is: appending to the whole string put the path inside the query, so
// http://host:4318/?token=x was posted to / with a query that ended in the path.
func (e Exporter) tracesURL() (string, error) {
	u, err := url.Parse(e.Endpoint)
	if err != nil {
		return "", err
	}
	u.Path = withTracesPath(u.Path)
	if u.RawPath != "" {
		u.RawPath = withTracesPath(u.RawPath)
	}
	return u.String(), nil
}

func withTracesPath(path string) string {
	path = strings.TrimRight(path, "/")
	if strings.HasSuffix(path, tracesPath) {
		return path
	}
	return path + tracesPath
}

// exportError is a failure whose text has already had the endpoint and the header
// values removed. It unwraps to the cause, so errors.Is still sees a deadline.
type exportError struct {
	text string
	err  error
}

func (e *exportError) Error() string { return e.text }
func (e *exportError) Unwrap() error { return e.err }

// scrub makes text safe to return or print: the endpoint is replaced by its
// redacted form wherever it appears, and the header values and the credentials
// inside the endpoint (its password and its query values) are masked, because a
// collector's refusal or a proxy's error can echo any of them back.
func (e Exporter) scrub(text, rawURL, safeURL string) string {
	text = strings.ReplaceAll(text, rawURL, safeURL)
	secrets := make([]string, 0, len(e.Headers)+2)
	for _, value := range e.Headers {
		secrets = append(secrets, value)
	}
	if u, err := url.Parse(e.Endpoint); err == nil {
		if u.User != nil {
			secrets = append(secrets, u.User.Username())
			if password, ok := u.User.Password(); ok {
				secrets = append(secrets, password)
			}
		}
		for _, values := range u.Query() {
			secrets = append(secrets, values...)
		}
	}
	return redact.String(text, secrets...)
}

// Export sends one trace and reports what the collector answered.
//
// A non-2xx response is an error: an exporter that reported success on a 403
// would turn a misconfigured collector into silent data loss, which is the
// failure mode observability work exists to prevent.
func (e Exporter) Export(ctx context.Context, trace Trace) (Delivery, error) {
	if strings.TrimSpace(e.Endpoint) == "" {
		return Delivery{}, fmt.Errorf("OTLP endpoint is required")
	}
	document, err := OTLP(trace)
	if err != nil {
		return Delivery{}, err
	}
	target, err := e.tracesURL()
	if err != nil {
		// url.Parse quotes what it rejected, which for a malformed endpoint is a
		// piece of its userinfo or query, so its text is not repeated.
		return Delivery{}, &exportError{text: fmt.Sprintf("OTLP endpoint %s is not a valid URL: remove spaces and control characters, and percent-encode any %%, /, # or ? in the userinfo", redact.URL(e.Endpoint))}
	}
	safeTarget := redact.URL(target)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(document))
	if err != nil {
		return Delivery{}, &exportError{text: e.scrub(fmt.Sprintf("build OTLP request for %s: %v", safeTarget, err), target, safeTarget), err: err}
	}
	request.Header.Set("Content-Type", "application/json")
	for key, value := range e.Headers {
		request.Header.Set(key, value)
	}
	client := e.Client
	if client == nil {
		timeout := e.Timeout
		if timeout <= 0 {
			timeout = DefaultExportTimeout
		}
		client = &http.Client{Timeout: timeout}
	}
	response, err := client.Do(request)
	if err != nil {
		// The *url.Error repeats the request URL, with only the password masked, so
		// the cause is reported on its own under the redacted address.
		cause := err
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Err != nil {
			cause = urlErr.Err
		}
		return Delivery{}, &exportError{text: e.scrub(fmt.Sprintf("post OTLP spans to %s: %v", safeTarget, cause), target, safeTarget), err: cause}
	}
	defer response.Body.Close()
	// Bounded read: a collector's error body is short, and an endpoint that
	// streams megabytes at us should not be able to exhaust memory.
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	if readErr != nil {
		return Delivery{}, &exportError{text: e.scrub(fmt.Sprintf("read collector response from %s: %v", safeTarget, readErr), target, safeTarget), err: readErr}
	}
	delivery := Delivery{
		Endpoint: safeTarget,
		TraceID:  trace.TraceID,
		Status:   response.StatusCode,
		Bytes:    len(document),
		Spans:    countSpans(trace),
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		delivery.Response = e.scrub(strings.TrimSpace(string(body)), target, safeTarget)
		return delivery, &exportError{text: fmt.Sprintf("collector at %s rejected the spans: HTTP %d %s", safeTarget, response.StatusCode, delivery.Response)}
	}
	delivery.Delivered = true
	return delivery, nil
}

// Delivery is the outcome of one export, reported so a caller can prove the
// spans actually left the process rather than only being formatted. TraceID
// names the trace the collector now holds (or refused), so a delivery can be
// matched to a worker's log line and to a search in the collector. Endpoint is
// the collector's address without its userinfo or query.
type Delivery struct {
	Endpoint  string `json:"endpoint"`
	TraceID   string `json:"trace_id"`
	Delivered bool   `json:"delivered"`
	Status    int    `json:"status"`
	Spans     int    `json:"spans"`
	Bytes     int    `json:"bytes"`
	Response  string `json:"response,omitempty"`
}

func countSpans(trace Trace) int {
	total := 0
	var walk func(*Span)
	walk = func(span *Span) {
		if span == nil {
			return
		}
		total++
		for _, child := range span.Children {
			walk(child)
		}
	}
	walk(trace.Root)
	return total
}
