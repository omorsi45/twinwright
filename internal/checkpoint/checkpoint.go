package checkpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"twinwright/internal/compiler"
	"twinwright/internal/store"
)

const FormatVersion = 1

type Checkpoint struct {
	ID             string `json:"id"`
	RunID          string `json:"run_id"`
	EventSeq       int    `json:"event_seq"`
	EventType      string `json:"event_type"`
	FormatVersion  int    `json:"format_version"`
	ManifestDigest string `json:"manifest_digest"`
	PrefixDigest   string `json:"prefix_digest"`
	ModelTurns     int    `json:"model_turns"`
	ToolCalls      int    `json:"tool_calls"`
}

// List describes committed restore boundaries in a paused or completed run.
func List(ctx context.Context, s *store.Store, runID string, manifest compiler.Manifest) ([]Checkpoint, error) {
	if err := compiler.ValidateManifest(manifest); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	run, err := s.Run(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.Status != "paused" && run.Status != "completed" {
		return nil, fmt.Errorf("run %s is %s; checkpoints require paused or completed run", runID, run.Status)
	}
	var digest string
	if err := s.DB.QueryRowContext(ctx, "SELECT digest FROM worlds WHERE id=?", run.WorldID).Scan(&digest); err != nil {
		return nil, err
	}
	if digest != manifest.Digest {
		return nil, fmt.Errorf("manifest mismatch for run %s", runID)
	}
	events, err := s.Events(ctx, runID)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("run %s has no events", runID)
	}
	h := sha256.New()
	io.WriteString(h, fmt.Sprintf("twinwright-checkpoint:%d:%s:%s\n", FormatVersion, runID, manifest.Digest))
	var points []Checkpoint
	var modelOpen, toolOpen bool
	var toolCallID, toolOperation string
	modelTurns, toolCalls := 0, 0
	for i, event := range events {
		if i == 0 && event.Type != "execution.started" {
			return nil, fmt.Errorf("run ledger lacks start event")
		}
		if event.RunID != runID || event.Seq != i+1 || event.ID != fmt.Sprintf("%s/%d", runID, i+1) {
			return nil, fmt.Errorf("ledger sequence or ID differs at position %d", i+1)
		}
		canonical, err := canonicalJSON(event.Payload)
		if err != nil {
			return nil, fmt.Errorf("event %d payload: %w", event.Seq, err)
		}
		entry, err := json.Marshal([]any{event.Seq, event.Type, event.RecordedAt, event.WorldAt, json.RawMessage(canonical)})
		if err != nil {
			return nil, err
		}
		h.Write(entry)
		h.Write([]byte{'\n'})
		boundary := false
		switch event.Type {
		case "execution.started":
			if i != 0 {
				return nil, fmt.Errorf("unexpected start event at sequence %d", event.Seq)
			}
			var started map[string]string
			if err := json.Unmarshal(event.Payload, &started); err != nil {
				return nil, err
			}
			if started["scenario"] != run.Scenario || started["provider"] != run.Provider || started["model"] != run.Model || started["world_id"] != run.WorldID {
				return nil, fmt.Errorf("execution start metadata differs from run")
			}
		case "model.request":
			if modelOpen || toolOpen {
				return nil, fmt.Errorf("overlapping model request at event %d", event.Seq)
			}
			modelOpen = true
		case "model.response":
			if !modelOpen || toolOpen {
				return nil, fmt.Errorf("model response without request at event %d", event.Seq)
			}
			var response struct {
				Role string `json:"role"`
			}
			if err := json.Unmarshal(event.Payload, &response); err != nil || response.Role != "assistant" {
				return nil, fmt.Errorf("invalid model response at event %d", event.Seq)
			}
			modelOpen = false
			modelTurns++
			boundary = true
		case "tool.request":
			if modelOpen || toolOpen {
				return nil, fmt.Errorf("overlapping tool request at event %d", event.Seq)
			}
			var request struct {
				CallID      string `json:"call_id"`
				OperationID string `json:"operation_id"`
			}
			if err := json.Unmarshal(event.Payload, &request); err != nil || request.CallID == "" || request.OperationID == "" {
				return nil, fmt.Errorf("invalid tool request at event %d", event.Seq)
			}
			toolOpen, toolCallID, toolOperation = true, request.CallID, request.OperationID
		case "state.mutation", "retry":
			if !toolOpen {
				return nil, fmt.Errorf("%s without tool request at event %d", event.Type, event.Seq)
			}
		case "error":
			var detail struct {
				Kind string `json:"kind"`
			}
			if err := json.Unmarshal(event.Payload, &detail); err != nil || !toolOpen || detail.Kind != "injected_503" {
				return nil, fmt.Errorf("unsupported error at event %d", event.Seq)
			}
		case "tool.response":
			if !toolOpen {
				return nil, fmt.Errorf("tool response without request at event %d", event.Seq)
			}
			var response struct {
				CallID      string `json:"call_id"`
				OperationID string `json:"operation_id"`
			}
			if err := json.Unmarshal(event.Payload, &response); err != nil || response.CallID != toolCallID || response.OperationID != toolOperation {
				return nil, fmt.Errorf("tool response does not match request at event %d", event.Seq)
			}
			toolOpen = false
			toolCalls++
			boundary = true
		case "execution.paused", "execution.completed":
			if modelOpen || toolOpen {
				return nil, fmt.Errorf("execution ended during action at event %d", event.Seq)
			}
			if event.Type == "execution.completed" && i != len(events)-1 {
				return nil, fmt.Errorf("completion event is not last")
			}
			boundary = true
		default:
			return nil, fmt.Errorf("unsupported event type %q at sequence %d", event.Type, event.Seq)
		}
		if boundary {
			prefix := hex.EncodeToString(h.Sum(nil))
			points = append(points, Checkpoint{
				ID: "CP-" + prefix[:20], RunID: runID, EventSeq: event.Seq, EventType: event.Type,
				FormatVersion: FormatVersion, ManifestDigest: manifest.Digest, PrefixDigest: prefix,
				ModelTurns: modelTurns, ToolCalls: toolCalls,
			})
		}
	}
	if modelOpen || toolOpen {
		return nil, fmt.Errorf("run ledger ends during an action")
	}
	if modelTurns != run.Step {
		return nil, fmt.Errorf("run step differs from model response count")
	}
	if run.Status == "paused" && events[len(events)-1].Type != "execution.paused" {
		return nil, fmt.Errorf("paused run lacks terminal pause event")
	}
	if run.Status == "completed" && events[len(events)-1].Type != "execution.completed" {
		return nil, fmt.Errorf("completed run lacks terminal completion event")
	}
	return points, nil
}

func Select(points []Checkpoint, eventSeq int) (Checkpoint, error) {
	var supported []string
	for _, point := range points {
		if point.EventSeq == eventSeq {
			return point, nil
		}
		supported = append(supported, fmt.Sprint(point.EventSeq))
	}
	return Checkpoint{}, fmt.Errorf("event %d is not a checkpoint; supported event sequences: %s", eventSeq, strings.Join(supported, ", "))
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("multiple JSON values")
	}
	return json.Marshal(value)
}
