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

// TestDeliveryReportsTheTraceID lets an operator correlate a delivery with the
// collector's index without re-deriving the ID, including when the collector
// refused the spans and the delivery is the only record of the attempt.
func TestDeliveryReportsTheTraceID(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		delivered bool
	}{
		{"accepted", http.StatusOK, true},
		{"rejected", http.StatusForbidden, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer collector.Close()
			built := sampleTrace()
			delivery, err := Exporter{Endpoint: collector.URL}.Export(context.Background(), built)
			if (err == nil) != tc.delivered {
				t.Fatalf("err=%v delivered=%v", err, tc.delivered)
			}
			if delivery.TraceID != built.TraceID {
				t.Fatalf("delivery trace id=%q want %q", delivery.TraceID, built.TraceID)
			}
			encoded, err := json.Marshal(delivery)
			if err != nil {
				t.Fatal(err)
			}
			if want := `"trace_id":"` + built.TraceID + `"`; !strings.Contains(string(encoded), want) {
				t.Fatalf("delivery JSON %s lacks %s", encoded, want)
			}
		})
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

// OTLP/HTTP answers 200 with partialSuccess.rejectedSpans when it kept some spans
// and refused others. The status code alone reads as a full delivery, which is
// the silent loss the exporter exists to prevent, so a rejected span is a
// failure. The count is a 64-bit integer, which the protobuf JSON mapping writes
// as a string; a number is accepted too.
func TestExporterReportsAPartialSuccess(t *testing.T) {
	const headerValue = "header-value-not-a-secret"
	for _, tc := range []struct {
		name      string
		body      string
		rejected  int
		delivered bool
		text      []string
	}{
		{"rejected spans as a string", `{"partialSuccess":{"rejectedSpans":"2","errorMessage":"2 spans are too old"}}`, 2, false,
			[]string{"2 of 3 spans", "2 spans are too old"}},
		{"rejected spans as a number", `{"partialSuccess":{"rejectedSpans":1}}`, 1, false, []string{"1 of 3 spans"}},
		{"every span rejected", `{"partialSuccess":{"rejectedSpans":"3","errorMessage":"quota"}}`, 3, false, []string{"3 of 3 spans", "quota"}},
		{"the reason is scrubbed", `{"partialSuccess":{"rejectedSpans":"1","errorMessage":"bad key ` + headerValue + `"}}`, 1, false, []string{"1 of 3 spans"}},
		// A message with nothing rejected is a warning, not a loss.
		{"zero rejected with a warning", `{"partialSuccess":{"rejectedSpans":"0","errorMessage":"deprecated field"}}`, 0, true, nil},
		{"empty partial success", `{"partialSuccess":{}}`, 0, true, nil},
		{"empty object", `{}`, 0, true, nil},
		{"empty body", ``, 0, true, nil},
		{"a body that is not JSON", `OK`, 0, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collector := newRecordingCollector(t, http.StatusOK, tc.body)
			exporter := Exporter{Endpoint: collector.URL, Headers: map[string]string{"X-Api-Key": headerValue}}
			delivery, err := exporter.Export(context.Background(), sampleTrace())
			if tc.delivered {
				if err != nil || !delivery.Delivered || delivery.RejectedSpans != 0 {
					t.Fatalf("delivery=%+v err=%v, want a full delivery", delivery, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("a partial success was reported as a delivery: %+v", delivery)
			}
			if delivery.Delivered || delivery.RejectedSpans != tc.rejected || delivery.Spans != 3 || delivery.Status != http.StatusOK {
				t.Fatalf("delivery=%+v, want not delivered, %d rejected of 3, status 200", delivery, tc.rejected)
			}
			for _, want := range tc.text {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %q, want it to say %q", err, want)
				}
			}
			encoded, jsonErr := json.Marshal(delivery)
			if jsonErr != nil {
				t.Fatal(jsonErr)
			}
			if want := `"rejected_spans":`; !strings.Contains(string(encoded), want) {
				t.Fatalf("delivery JSON %s lacks %s", encoded, want)
			}
			for _, text := range []string{err.Error(), delivery.Response, string(encoded)} {
				if strings.Contains(text, headerValue) {
					t.Fatalf("a header value reached the report: %s", text)
				}
			}
		})
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
