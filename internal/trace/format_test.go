package trace

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"twinwright/internal/agent"
	"twinwright/internal/checkpoint"
	"twinwright/internal/dispatch"
	"twinwright/internal/fork"
	"twinwright/internal/store"
)

func TestOTLPEncodesLedgerSpans(t *testing.T) {
	s := newStore(t)
	run := execute(t, s, billingManifest(t), "duplicate-charge", "fixture-v1", store.RunOptions{FaultOperation: "listCharges"}, agent.ScriptedProvider{})
	encoded, err := OTLP(build(t, s, run.ID))
	if err != nil {
		t.Fatal(err)
	}
	var doc otlpDocument
	if err := json.Unmarshal(encoded, &doc); err != nil {
		t.Fatal(err)
	}
	spans := doc.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) < 3 {
		t.Fatalf("spans=%d", len(spans))
	}
	root := spans[0]
	if len(root.TraceID) != 32 || len(root.SpanID) != 16 || root.ParentSpanID != "" || root.Name != "run" || root.Status.Code != 1 {
		t.Fatalf("root=%+v", root)
	}
	if !digits(root.StartTimeUnixNano) || !digits(root.EndTimeUnixNano) {
		t.Fatalf("times=%s %s", root.StartTimeUnixNano, root.EndTimeUnixNano)
	}
	var failed *otlpSpan
	for i := range spans {
		if spans[i].Name != "run" && spans[i].ParentSpanID != root.SpanID {
			t.Fatalf("span %s parent=%s", spans[i].Name, spans[i].ParentSpanID)
		}
		if spans[i].Name == "tool.call" && attr(&spans[i], "http.status") == "503" {
			failed = &spans[i]
		}
	}
	if failed == nil || failed.Status.Code != 2 || attr(failed, "error.type") != "server_error" {
		t.Fatalf("failed span=%+v", failed)
	}
	if valueType(failed, "http.status") != "intValue" || valueType(spansFor(spans, "evaluation"), "passed") != "boolValue" {
		t.Fatalf("attribute types status=%s passed=%s", valueType(failed, "http.status"), valueType(spansFor(spans, "evaluation"), "passed"))
	}
}

func TestOTLPLinksForkToParentRoot(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "world.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	manifest := billingManifest(t)
	parent := execute(t, s, manifest, "duplicate-charge", "fixture-v1", store.RunOptions{}, agent.ScriptedProvider{})
	source, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	points, err := checkpoint.List(ctx, source, parent.ID, manifest)
	if err != nil {
		t.Fatal(err)
	}
	created, err := fork.Create(ctx, source, s, points[len(points)-1], manifest, fork.Options{})
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.Runner{Store: s, Dispatch: &dispatch.Dispatcher{Store: s, Manifest: manifest}, Manifest: manifest, Provider: agent.ScriptedProvider{}}
	if _, err := runner.Execute(ctx, created.Run.ID, 1); err != nil {
		t.Fatal(err)
	}
	parentDoc, childDoc := otlpDocument{}, otlpDocument{}
	parentEncoded, err := OTLP(build(t, s, parent.ID))
	if err != nil {
		t.Fatal(err)
	}
	childEncoded, err := OTLP(build(t, s, created.Run.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(parentEncoded, &parentDoc); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(childEncoded, &childDoc); err != nil {
		t.Fatal(err)
	}
	parentRoot := parentDoc.ResourceSpans[0].ScopeSpans[0].Spans[0]
	childRoot := childDoc.ResourceSpans[0].ScopeSpans[0].Spans[0]
	if len(childRoot.Links) != 1 || childRoot.Links[0].TraceID != parentRoot.TraceID || childRoot.Links[0].SpanID != parentRoot.SpanID {
		t.Fatalf("link=%+v parent=%s/%s", childRoot.Links, parentRoot.TraceID, parentRoot.SpanID)
	}
}

func TestTextTreeShowsToolOutcome(t *testing.T) {
	s := newStore(t)
	run := execute(t, s, billingManifest(t), "duplicate-charge", "fixture-v1", store.RunOptions{FaultOperation: "listCharges"}, agent.ScriptedProvider{})
	text := Text(build(t, s, run.ID))
	lines := strings.Split(text, "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "run "+run.ID+" completed") || !strings.HasPrefix(lines[1], "  model.invocation") {
		t.Fatalf("trace does not open with the run and its first model call:\n%s", text)
	}
	for _, want := range []string{
		"  tool.call listCharges status=503 server_error",
		"    fault.injected",
		"  evaluation passed",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

type otlpDocument struct {
	ResourceSpans []struct {
		ScopeSpans []struct {
			Spans []otlpSpan `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

type otlpSpan struct {
	TraceID           string `json:"traceId"`
	SpanID            string `json:"spanId"`
	ParentSpanID      string `json:"parentSpanId"`
	Name              string `json:"name"`
	StartTimeUnixNano string `json:"startTimeUnixNano"`
	EndTimeUnixNano   string `json:"endTimeUnixNano"`
	Attributes        []struct {
		Key   string         `json:"key"`
		Value map[string]any `json:"value"`
	} `json:"attributes"`
	Status struct {
		Code int `json:"code"`
	} `json:"status"`
	Links []struct {
		TraceID string `json:"traceId"`
		SpanID  string `json:"spanId"`
	} `json:"links"`
}

func spansFor(spans []otlpSpan, name string) *otlpSpan {
	for i := range spans {
		if spans[i].Name == name {
			return &spans[i]
		}
	}
	return nil
}

func attr(span *otlpSpan, key string) string {
	if span == nil {
		return ""
	}
	for _, attribute := range span.Attributes {
		if attribute.Key == key {
			for _, value := range attribute.Value {
				return toString(value)
			}
		}
	}
	return ""
}

func valueType(span *otlpSpan, key string) string {
	if span == nil {
		return ""
	}
	for _, attribute := range span.Attributes {
		if attribute.Key == key {
			for kind := range attribute.Value {
				return kind
			}
		}
	}
	return ""
}

func toString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	default:
		encoded, _ := json.Marshal(v)
		return string(encoded)
	}
}

func digits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
