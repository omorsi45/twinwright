package trace

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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
type Exporter struct {
	// Endpoint is the collector's base URL, for example
	// http://127.0.0.1:4318. The OTLP traces path is appended when absent.
	Endpoint string
	// Headers are sent with the request, for a collector behind an auth proxy.
	Headers map[string]string
	// Client defaults to an http.Client with Timeout set.
	Client *http.Client
	// Timeout applies when Client is nil.
	Timeout time.Duration
}

// tracesURL appends the OTLP/HTTP traces path unless the caller already gave a
// full path, so both forms work.
func (e Exporter) tracesURL() string {
	endpoint := strings.TrimRight(e.Endpoint, "/")
	if strings.HasSuffix(endpoint, "/v1/traces") {
		return endpoint
	}
	return endpoint + "/v1/traces"
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
	url := e.tracesURL()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(document))
	if err != nil {
		return Delivery{}, err
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
		return Delivery{}, fmt.Errorf("post OTLP spans to %s: %w", url, err)
	}
	defer response.Body.Close()
	// Bounded read: a collector's error body is short, and an endpoint that
	// streams megabytes at us should not be able to exhaust memory.
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	if readErr != nil {
		return Delivery{}, fmt.Errorf("read collector response from %s: %w", url, readErr)
	}
	delivery := Delivery{
		Endpoint: url,
		Status:   response.StatusCode,
		Bytes:    len(document),
		Spans:    countSpans(trace),
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		delivery.Response = strings.TrimSpace(string(body))
		return delivery, fmt.Errorf("collector at %s rejected the spans: HTTP %d %s", url, response.StatusCode, delivery.Response)
	}
	delivery.Delivered = true
	return delivery, nil
}

// Delivery is the outcome of one export, reported so a caller can prove the
// spans actually left the process rather than only being formatted.
type Delivery struct {
	Endpoint  string `json:"endpoint"`
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
