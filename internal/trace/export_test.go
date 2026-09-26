package trace

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sampleTrace() Trace {
	return Trace{
		TraceID: "0123456789abcdef0123456789abcdef",
		Root: &Span{
			SpanID: "0123456789abcdef", Name: "run", Start: "2026-09-26T12:00:00Z", End: "2026-09-26T12:00:01Z",
			Status: "ok", Attributes: map[string]any{"run.id": "R-1"},
			Children: []*Span{
				{SpanID: "1123456789abcdef", Name: "model", Start: "2026-09-26T12:00:00Z", End: "2026-09-26T12:00:00.5Z", Status: "ok"},
				{SpanID: "2123456789abcdef", Name: "tool", Start: "2026-09-26T12:00:00.5Z", End: "2026-09-26T12:00:01Z", Status: "ok"},
			},
		},
	}
}

func TestExporterPostsOTLPToACollector(t *testing.T) {
	var gotPath, gotContentType, gotHeader string
	var gotBody []byte
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotHeader = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"partialSuccess":{}}`))
	}))
	defer collector.Close()

	exporter := Exporter{Endpoint: collector.URL, Headers: map[string]string{"Authorization": "Bearer token"}}
	delivery, err := exporter.Export(context.Background(), sampleTrace())
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !delivery.Delivered || delivery.Status != http.StatusOK {
		t.Fatalf("delivery=%+v", delivery)
	}
	if delivery.Spans != 3 {
		t.Fatalf("spans=%d want 3", delivery.Spans)
	}
	if gotPath != "/v1/traces" {
		t.Fatalf("path=%q want /v1/traces", gotPath)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content type=%q", gotContentType)
	}
	if gotHeader != "Bearer token" {
		t.Fatalf("custom header was dropped: %q", gotHeader)
	}
	// What arrived must be a real OTLP document, not an internal shape.
	var document map[string]any
	if err = json.Unmarshal(gotBody, &document); err != nil {
		t.Fatalf("collector received invalid JSON: %v", err)
	}
	if _, ok := document["resourceSpans"]; !ok {
		t.Fatalf("collector received a document without resourceSpans: %s", truncate(gotBody))
	}
}

func TestExporterAcceptsAFullTracesPath(t *testing.T) {
	var hits int
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/v1/traces" {
			t.Errorf("path=%q", r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer collector.Close()
	exporter := Exporter{Endpoint: collector.URL + "/v1/traces"}
	if _, err := exporter.Export(context.Background(), sampleTrace()); err != nil {
		t.Fatalf("export: %v", err)
	}
	if hits != 1 {
		t.Fatalf("hits=%d", hits)
	}
}

// A collector that rejects the spans must surface as an error. Reporting
// success on a 4xx would turn a misconfigured collector into silent data loss.
func TestExporterTreatsRejectionAsFailure(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("missing tenant header"))
	}))
	defer collector.Close()
	delivery, err := Exporter{Endpoint: collector.URL}.Export(context.Background(), sampleTrace())
	if err == nil {
		t.Fatal("a 403 from the collector was reported as success")
	}
	if delivery.Delivered {
		t.Fatalf("delivery claims success: %+v", delivery)
	}
	if delivery.Status != http.StatusForbidden {
		t.Fatalf("status=%d", delivery.Status)
	}
	if !strings.Contains(delivery.Response, "missing tenant header") {
		t.Fatalf("the collector's reason was lost: %q", delivery.Response)
	}
}

func TestExporterRequiresAnEndpoint(t *testing.T) {
	if _, err := (Exporter{}).Export(context.Background(), sampleTrace()); err == nil {
		t.Fatal("expected an explicit missing-endpoint error")
	}
}

func TestExporterReportsAnUnreachableCollector(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := collector.URL
	collector.Close()
	if _, err := (Exporter{Endpoint: endpoint}).Export(context.Background(), sampleTrace()); err == nil {
		t.Fatal("expected a transport error for a closed collector")
	}
}

func truncate(b []byte) string {
	if len(b) > 400 {
		return string(b[:400]) + "…"
	}
	return string(b)
}
