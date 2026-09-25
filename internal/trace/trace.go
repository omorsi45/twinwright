// Package trace projects a run's event ledger into a span tree. It only reads,
// and the same ledger always yields the same trace.
package trace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"twinwright/internal/eval"
	"twinwright/internal/redact"
	"twinwright/internal/store"
)

type Trace struct {
	TraceID string  `json:"trace_id"`
	Summary Summary `json:"summary"`
	Root    *Span   `json:"root"`
}

// Span covers the events from StartSeq to EndSeq. Times are wall-clock commit
// times from the ledger.
type Span struct {
	SpanID     string         `json:"span_id"`
	Name       string         `json:"name"`
	StartSeq   int            `json:"start_seq"`
	EndSeq     int            `json:"end_seq"`
	Start      string         `json:"start"`
	End        string         `json:"end"`
	DurationMS float64        `json:"duration_ms"`
	Status     string         `json:"status"`
	Attributes map[string]any `json:"attributes"`
	Events     []Event        `json:"events,omitempty"`
	Children   []*Span        `json:"children,omitempty"`
}

type Event struct {
	Seq        int            `json:"seq"`
	Name       string         `json:"name"`
	Time       string         `json:"time"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

type TokenUsage struct {
	Recorded     bool `json:"recorded"`
	InputTokens  int  `json:"input_tokens"`
	OutputTokens int  `json:"output_tokens"`
	TotalTokens  int  `json:"total_tokens"`
}

type Summary struct {
	RunID                string       `json:"run_id"`
	ParentRunID          string       `json:"parent_run_id,omitempty"`
	ForkEventSeq         int          `json:"fork_event_seq,omitempty"`
	Scenario             string       `json:"scenario"`
	Provider             string       `json:"provider"`
	Model                string       `json:"model"`
	Principal            string       `json:"principal"`
	WorldID              string       `json:"world_id"`
	Status               string       `json:"status"`
	Events               int          `json:"events"`
	ModelTurns           int          `json:"model_turns"`
	ToolCalls            int          `json:"tool_calls"`
	FailedToolCalls      int          `json:"failed_tool_calls"`
	StateMutations       int          `json:"state_mutations"`
	Retries              int          `json:"retries"`
	Faults               int          `json:"faults"`
	AuthorizationAllowed int          `json:"authorization_allowed"`
	AuthorizationDenied  int          `json:"authorization_denied"`
	Errors               int          `json:"errors"`
	WallClockMS          float64      `json:"wall_clock_ms"`
	ModelMS              float64      `json:"model_ms"`
	ToolMS               float64      `json:"tool_ms"`
	SimulatedLatencyMS   int          `json:"simulated_latency_ms"`
	TokenUsage           TokenUsage   `json:"token_usage"`
	Evaluation           *eval.Report `json:"evaluation,omitempty"`
}

// Build reads one run and returns its trace.
func Build(ctx context.Context, s *store.Store, runID string) (Trace, error) {
	run, err := s.Run(ctx, runID)
	if err != nil {
		return Trace{}, err
	}
	events, err := s.Events(ctx, runID)
	if err != nil {
		return Trace{}, err
	}
	if len(events) == 0 {
		return Trace{}, fmt.Errorf("run %s has no events", runID)
	}
	b := builder{run: run, summary: Summary{RunID: run.ID, Scenario: run.Scenario, Provider: run.Provider, Model: run.Model,
		Principal: run.PrincipalID, WorldID: run.WorldID, Status: run.Status, Events: len(events)}}
	b.root = b.span("run", events[0])
	b.root.Attributes = map[string]any{"run.id": run.ID, "world.id": run.WorldID, "scenario": run.Scenario,
		"provider": run.Provider, "model": run.Model, "principal": run.PrincipalID, "run.status": run.Status}
	lineage, err := s.Lineage(ctx, runID)
	if err != nil && err != sql.ErrNoRows {
		return Trace{}, err
	}
	if err == nil {
		b.root.Attributes["parent.run_id"] = lineage.ParentRunID
		b.root.Attributes["fork.event_seq"] = lineage.ForkEventSeq
		b.summary.ParentRunID, b.summary.ForkEventSeq = lineage.ParentRunID, lineage.ForkEventSeq
	}
	b.checkpoints = events[0].Type == "execution.started"
	for _, event := range events {
		if err := b.add(event); err != nil {
			return Trace{}, fmt.Errorf("event %d: %w", event.Seq, err)
		}
	}
	last := events[len(events)-1]
	b.close(b.root, last)
	switch run.Status {
	case "completed":
		b.root.Status = "ok"
	case "failed":
		b.root.Status = "error"
	}
	if run.Status == "completed" {
		report, err := eval.Evaluate(ctx, s, run.WorldID, run.Scenario)
		if err != nil {
			return Trace{}, err
		}
		b.summary.Evaluation = &report
		evaluation := b.span("evaluation", last)
		b.close(evaluation, last)
		var failed []string
		for _, check := range report.Checks {
			if !check.Passed {
				failed = append(failed, check.Name)
			}
		}
		evaluation.Attributes = map[string]any{"passed": report.Passed, "computed_at_read_time": true}
		if len(failed) > 0 {
			evaluation.Attributes["failed_checks"] = failed
		}
		evaluation.Status = "ok"
		if !report.Passed {
			evaluation.Status = "error"
		}
		b.root.Children = append(b.root.Children, evaluation)
	}
	b.summary.WallClockMS = b.root.DurationMS
	b.summary.ModelMS = roundMS(b.summary.ModelMS)
	b.summary.ToolMS = roundMS(b.summary.ToolMS)
	return Trace{TraceID: hashID("twinwright-trace:"+run.ID, 32), Summary: b.summary, Root: b.root}, nil
}

func roundMS(value float64) float64 {
	return math.Round(value*1000) / 1000
}

type builder struct {
	run         store.Run
	root        *Span
	current     *Span
	lastModel   *Span
	checkpoints bool
	summary     Summary
}

func (b *builder) span(name string, event store.Event) *Span {
	return &Span{SpanID: hashID(fmt.Sprintf("%s:%s:%d", b.run.ID, name, event.Seq), 16), Name: name, StartSeq: event.Seq,
		Start: event.RecordedAt, Status: "unset", Attributes: map[string]any{}}
}

func (b *builder) close(span *Span, event store.Event) {
	span.EndSeq, span.End = event.Seq, event.RecordedAt
	start, errStart := time.Parse(time.RFC3339Nano, span.Start)
	end, errEnd := time.Parse(time.RFC3339Nano, span.End)
	if errStart == nil && errEnd == nil {
		span.DurationMS = math.Round(float64(end.Sub(start).Microseconds())) / 1000
	}
}

func (b *builder) note(span *Span, event store.Event, name string, attributes map[string]any) {
	span.Events = append(span.Events, Event{Seq: event.Seq, Name: name, Time: event.RecordedAt, Attributes: attributes})
}

func (b *builder) add(event store.Event) error {
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(event.Payload))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return err
	}
	switch event.Type {
	case "execution.started":
	case "execution.forked":
		b.note(b.root, event, "fork", pick(payload, "parent_run_id", "fork_event_seq", "checkpoint_id"))
	case "observation.overridden":
		b.note(b.root, event, "observation.overridden", pick(payload, "call_id", "operation_id", "original_status", "status"))
	case "execution.paused", "execution.completed":
		attributes := map[string]any{}
		if b.checkpoints {
			attributes["checkpoint"] = true
		}
		b.note(b.root, event, event.Type, attributes)
	case "model.request":
		span := b.span("model.invocation", event)
		span.Attributes["provider"], span.Attributes["model"] = payload["provider"], payload["model"]
		if history, ok := payload["history"].([]any); ok {
			span.Attributes["history_messages"] = len(history)
		}
		if operations, ok := payload["operations"].([]any); ok {
			span.Attributes["operations_exposed"] = len(operations)
		}
		b.root.Children = append(b.root.Children, span)
		b.current, b.lastModel = span, span
	case "model.response":
		span := b.current
		if span == nil || span.Name != "model.invocation" {
			return fmt.Errorf("model response without request")
		}
		b.close(span, event)
		b.current = nil
		span.Status = "ok"
		b.summary.ModelTurns++
		b.summary.ModelMS += span.DurationMS
		if calls, ok := payload["tool_calls"].([]any); ok {
			span.Attributes["tool_calls"] = len(calls)
		}
		if usage, ok := payload["usage"].(map[string]any); ok {
			input, output, total := number(usage["input_tokens"]), number(usage["output_tokens"]), number(usage["total_tokens"])
			span.Attributes["tokens.input"], span.Attributes["tokens.output"], span.Attributes["tokens.total"] = input, output, total
			b.summary.TokenUsage.Recorded = true
			b.summary.TokenUsage.InputTokens += input
			b.summary.TokenUsage.OutputTokens += output
			b.summary.TokenUsage.TotalTokens += total
		}
		if b.checkpoints {
			span.Attributes["checkpoint"] = true
		}
	case "tool.request":
		span := b.span("tool.call", event)
		span.Attributes["call_id"], span.Attributes["operation"] = payload["call_id"], payload["operation_id"]
		b.root.Children = append(b.root.Children, span)
		b.current = span
	case "authorization.allowed", "authorization.denied":
		span, err := b.tool()
		if err != nil {
			return err
		}
		attributes := pick(payload, "principal_id", "permission", "call_index", "reason")
		attributes["principal"] = attributes["principal_id"]
		delete(attributes, "principal_id")
		attributes["decision"] = "allowed"
		if event.Type == "authorization.denied" {
			attributes["decision"] = "denied"
			span.Attributes["authorization.denied"] = true
			b.summary.AuthorizationDenied++
		} else {
			b.summary.AuthorizationAllowed++
		}
		b.note(span, event, "authorization.check", attributes)
	case "chaos.injected":
		span, err := b.tool()
		if err != nil {
			return err
		}
		attributes := pick(payload, "rule_id", "type", "status", "duration_ms", "matching_call")
		if latency := number(payload["duration_ms"]); latency > 0 {
			span.Attributes["simulated_latency_ms"] = latency
			b.summary.SimulatedLatencyMS += latency
		}
		b.summary.Faults++
		b.note(span, event, "fault.injected", attributes)
	case "chaos.actor_mutation":
		span, err := b.tool()
		if err != nil {
			return err
		}
		b.note(span, event, "chaos.actor_mutation", pick(payload, "rule_id", "operation_id", "status"))
	case "retry":
		span, err := b.tool()
		if err != nil {
			return err
		}
		b.summary.Retries++
		b.note(span, event, "retry", pick(payload, "call_id", "operation_id"))
	case "state.mutation":
		span, err := b.tool()
		if err != nil {
			return err
		}
		attributes := map[string]any{}
		for key, value := range payload {
			switch value.(type) {
			case string, json.Number, bool:
				attributes["mutation."+key] = scalar(value)
			}
		}
		b.summary.StateMutations++
		b.note(span, event, "state.mutation", attributes)
	case "error":
		kind, _ := payload["kind"].(string)
		switch {
		case kind == "injected_503" && b.current != nil && b.current.Name == "tool.call":
			b.summary.Faults++
			b.note(b.current, event, "fault.injected", map[string]any{"type": "legacy_503"})
		case kind == "provider" && b.lastModel != nil:
			b.lastModel.Status = "error"
			b.lastModel.Attributes["error.type"] = "provider"
			b.lastModel.Attributes["error.message"] = message(payload)
			b.summary.Errors++
		default:
			b.summary.Errors++
			b.note(b.root, event, "error", map[string]any{"error.type": kind, "error.message": message(payload)})
		}
	case "tool.response":
		span, err := b.tool()
		if err != nil {
			return err
		}
		b.close(span, event)
		b.current = nil
		status := number(payload["status"])
		span.Attributes["http.status"] = status
		b.summary.ToolCalls++
		b.summary.ToolMS += span.DurationMS
		span.Status = "ok"
		if status < 200 || status >= 300 {
			span.Status = "error"
			span.Attributes["error.type"] = errorType(status, span.Attributes["authorization.denied"] == true)
			b.summary.FailedToolCalls++
		}
		delete(span.Attributes, "authorization.denied")
		if b.checkpoints {
			span.Attributes["checkpoint"] = true
		}
	default:
		b.note(b.root, event, event.Type, nil)
	}
	return nil
}

func (b *builder) tool() (*Span, error) {
	if b.current == nil || b.current.Name != "tool.call" {
		return nil, fmt.Errorf("tool event outside a tool call")
	}
	return b.current, nil
}

func errorType(status int, denied bool) string {
	switch {
	case status == 0:
		return "transport_timeout"
	case denied:
		return "authorization_denied"
	case status == 429:
		return "rate_limited"
	case status >= 400 && status < 500:
		return "client_error"
	case status >= 500:
		return "server_error"
	}
	return "unexpected_status"
}

func message(payload map[string]any) string {
	text, _ := payload["message"].(string)
	return redact.String(text)
}

func pick(payload map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, key := range keys {
		if value, ok := payload[key]; ok {
			out[key] = scalar(value)
		}
	}
	return out
}

// scalar turns decoded JSON numbers into ints where they are whole, so trace
// attributes have stable types.
func scalar(value any) any {
	if n, ok := value.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
		if f, err := n.Float64(); err == nil {
			return f
		}
		return n.String()
	}
	return value
}

func number(value any) int {
	n, _ := scalar(value).(int)
	return n
}

func hashID(input string, length int) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:length]
}
