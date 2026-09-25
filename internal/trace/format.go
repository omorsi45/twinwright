package trace

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Text renders a trace as an indented tree.
func Text(trace Trace) string {
	var b strings.Builder
	writeSpan(&b, trace.Root, 0)
	return b.String()
}

func writeSpan(b *strings.Builder, span *Span, depth int) {
	indent := strings.Repeat("  ", depth)
	fmt.Fprintf(b, "%s%s", indent, span.Name)
	switch span.Name {
	case "run":
		fmt.Fprintf(b, " %v %v", span.Attributes["run.id"], span.Attributes["run.status"])
	case "model.invocation":
		fmt.Fprintf(b, " %v/%v", span.Attributes["provider"], span.Attributes["model"])
	case "tool.call":
		fmt.Fprintf(b, " %v status=%v", span.Attributes["operation"], span.Attributes["http.status"])
		if kind, ok := span.Attributes["error.type"]; ok {
			fmt.Fprintf(b, " %v", kind)
		}
	case "evaluation":
		if span.Attributes["passed"] == true {
			b.WriteString(" passed")
		} else {
			b.WriteString(" failed")
		}
	}
	if span.DurationMS > 0 {
		fmt.Fprintf(b, " %.3fms", span.DurationMS)
	}
	b.WriteByte('\n')
	for _, event := range span.Events {
		switch event.Name {
		case "execution.started", "execution.paused", "execution.completed":
			continue
		}
		fmt.Fprintf(b, "%s  %s\n", indent, event.Name)
	}
	for _, child := range span.Children {
		writeSpan(b, child, depth+1)
	}
}

// OTLP encodes a trace as one OTLP JSON resource-span document.
func OTLP(trace Trace) ([]byte, error) {
	var spans []map[string]any
	flatten(&spans, trace.TraceID, "", trace.Root)
	document := map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource": map[string]any{"attributes": otlpAttributes(map[string]any{
				"service.name":        "twinwright",
				"twinwright.run_id":   trace.Summary.RunID,
				"twinwright.scenario": trace.Summary.Scenario,
			})},
			"scopeSpans": []any{map[string]any{
				"scope": map[string]any{"name": "twinwright"},
				"spans": spans,
			}},
		}},
	}
	return json.Marshal(document)
}

func flatten(out *[]map[string]any, traceID, parentID string, span *Span) {
	encoded := map[string]any{
		"traceId":           traceID,
		"spanId":            span.SpanID,
		"name":              span.Name,
		"kind":              1,
		"startTimeUnixNano": unixNano(span.Start),
		"endTimeUnixNano":   unixNano(span.End),
		"attributes":        otlpAttributes(span.Attributes),
		"status":            map[string]any{"code": statusCode(span.Status)},
	}
	if parentID != "" {
		encoded["parentSpanId"] = parentID
	}
	if len(span.Events) > 0 {
		events := make([]any, 0, len(span.Events))
		for _, event := range span.Events {
			item := map[string]any{"timeUnixNano": unixNano(event.Time), "name": event.Name}
			if len(event.Attributes) > 0 {
				item["attributes"] = otlpAttributes(event.Attributes)
			}
			events = append(events, item)
		}
		encoded["events"] = events
	}
	if parent, ok := span.Attributes["parent.run_id"].(string); ok && parent != "" && span.Name == "run" {
		encoded["links"] = []any{map[string]any{
			"traceId": hashID("twinwright-trace:"+parent, 32),
			"spanId":  hashID(parent+":run:1", 16),
		}}
	}
	*out = append(*out, encoded)
	for _, child := range span.Children {
		flatten(out, traceID, span.SpanID, child)
	}
}

func statusCode(status string) int {
	switch status {
	case "ok":
		return 1
	case "error":
		return 2
	default:
		return 0
	}
}

func unixNano(recorded string) string {
	parsed, err := time.Parse(time.RFC3339Nano, recorded)
	if err != nil {
		return "0"
	}
	return strconv.FormatInt(parsed.UnixNano(), 10)
}

func otlpAttributes(attributes map[string]any) []any {
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]any, 0, len(keys))
	for _, key := range keys {
		out = append(out, map[string]any{"key": key, "value": otlpValue(attributes[key])})
	}
	return out
}

func otlpValue(value any) map[string]any {
	switch v := value.(type) {
	case bool:
		return map[string]any{"boolValue": v}
	case int:
		return map[string]any{"intValue": strconv.Itoa(v)}
	case int64:
		return map[string]any{"intValue": strconv.FormatInt(v, 10)}
	case float64:
		return map[string]any{"doubleValue": v}
	case []string:
		items := make([]any, len(v))
		for i, item := range v {
			items[i] = map[string]any{"stringValue": item}
		}
		return map[string]any{"arrayValue": map[string]any{"values": items}}
	default:
		return map[string]any{"stringValue": fmt.Sprint(v)}
	}
}
